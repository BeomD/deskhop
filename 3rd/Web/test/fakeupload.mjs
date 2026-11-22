// 3rd/Web/test/fakeupload.mjs
//
// serve.ps1 의 /upload 엔드포인트를 메모리에서 흉내 내는 테스트 더블.
// HttpSink 가 실제 네트워크/Windows 없이도 "이어받기 · 409 · finalize 중복 이름"
// 규칙을 그대로 검증할 수 있도록 serve.ps1 핸들러와 같은 의미론을 구현한다.
//
//   GET  /upload/config                       → { ok, root, port }
//   GET  /upload/status?path=<rel>            → { ok, part:<.part 크기> }
//   GET  /upload/part?path=<rel>&max=<n>      → .part 앞부분 원본 바이트(없으면 404)
//   POST /upload?path=<rel>&offset=<n>        → offset 기록. offset>cur 면 409
//   POST /upload/finalize?path=<rel>          → .part → 최종 이름(중복 시 -1)
//   POST /upload/abort?path=<rel>             → .part 삭제
//   POST /upload/mkdir?path=<rel>             → 폴더 생성

const EMPTY = new Uint8Array(0);

function toBytes(b) {
  if (!b) return EMPTY;
  if (b instanceof Uint8Array) return b;
  if (b instanceof ArrayBuffer) return new Uint8Array(b);
  if (ArrayBuffer.isView(b)) return new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
  if (typeof b === 'string') return new TextEncoder().encode(b);
  return EMPTY;
}

const parentOf = (rel) => { const i = rel.lastIndexOf('/'); return i < 0 ? '' : rel.slice(0, i); };
const baseOf = (rel) => rel.slice(rel.lastIndexOf('/') + 1);

export class FakeUpload {
  constructor(opts = {}) {
    this.root = opts.root || 'C:\\DeskHopWeb';
    this.port = opts.port || 9000;
    this.files = new Map();   // rel -> Uint8Array (확정된 파일)
    this.parts = new Map();   // rel -> Uint8Array (.part)
    this.dirs = new Set();    // rel
    this.requests = [];       // 'POST /upload?…' 감사 로그
    this.failNext = 0;        // 다음 N 개의 /upload POST 를 네트워크 오류로 실패시킨다
    this.seq = 0;             // 요청 번호(로그용)
  }

  // ---- 조회 헬퍼 -------------------------------------------------------
  exists(rel) { return this.files.has(rel) || this.parts.has(rel) || this.dirs.has(rel); }
  has(rel) { return this.files.has(rel); }
  readBytes(rel) { return this.files.get(rel) || null; }
  partBytes(rel) { return this.parts.get(rel) || null; }
  list() {
    const out = [];
    for (const k of this.files.keys()) out.push(k);
    for (const k of this.dirs.keys()) out.push(k + '/');
    return out.sort();
  }

  // ---- 응답 헬퍼 -------------------------------------------------------
  _res(status, bytes) {
    const body = toBytes(bytes);
    return {
      ok: status >= 200 && status < 300,
      status,
      async json() { return JSON.parse(new TextDecoder().decode(body)); },
      async arrayBuffer() { return body.buffer.slice(body.byteOffset, body.byteOffset + body.byteLength); },
    };
  }
  _json(status, obj) { return this._res(status, new TextEncoder().encode(JSON.stringify(obj))); }
  _err(status, msg) { return this._json(status, { ok: false, error: msg }); }

  _badPath(rel) {
    if (!rel) return true;
    return rel.split('/').some((seg) => seg === '.' || seg === '..' || seg === '');
  }

  // 같은 이름이 있으면 name-1.ext … (serve.ps1 의 Get-UniqueName 과 동일)
  _uniqueName(rel) {
    const dir = parentOf(rel), name = baseOf(rel);
    const join = (n) => (dir ? dir + '/' + n : n);
    if (!this.exists(join(name))) return name;
    const dot = name.lastIndexOf('.');
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : '';
    for (let i = 1; i < 10000; i++) {
      const cand = `${base}-${i}${ext}`;
      if (!this.exists(join(cand))) return cand;
    }
    return `${base}-${Date.now()}${ext}`;
  }

  _ensureDirs(rel, isFile) {
    let dir = isFile ? parentOf(rel) : rel;
    while (dir) { this.dirs.add(dir); dir = parentOf(dir); }
  }

  // ---- fetch 대역 ------------------------------------------------------
  async fetch(input, init = {}) {
    const url = new URL(String(input), `http://localhost:${this.port}/`);
    const path = url.pathname;
    const q = url.searchParams;
    const method = (init.method || 'GET').toUpperCase();
    this.requests.push(`${++this.seq}:${method} ${path}`);

    const rel = q.get('path') || '';
    const needsPath = path !== '/upload/config';

    if (method === 'GET' && path === '/upload/config') {
      return this._json(200, { ok: true, root: this.root, port: this.port });
    }

    if (needsPath && this._badPath(rel)) return this._err(400, 'bad path');

    if (method === 'GET' && path === '/upload/status') {
      return this._json(200, { ok: true, part: (this.parts.get(rel) || EMPTY).length });
    }

    if (method === 'GET' && path === '/upload/part') {
      const part = this.parts.get(rel);
      if (!part) return this._err(404, 'no part');
      let max = Number(q.get('max') || 0);
      if (!Number.isFinite(max) || max <= 0 || max > part.length) max = part.length;
      return this._res(200, part.subarray(0, max));
    }

    // 여기부터는 POST 만
    if (method !== 'POST') return this._err(405, 'method not allowed');

    if (path === '/upload') {
      if (this.failNext > 0) {                       // 네트워크 단절 흉내
        this.failNext--;
        throw new Error('simulated upload failure');
      }
      const body = toBytes(init.body);
      const cur = this.parts.get(rel) || EMPTY;
      let offset = Number(q.get('offset') || 0);
      if (!Number.isFinite(offset) || offset < 0) offset = 0;
      if (q.get('truncate') === '1') { this.parts.set(rel, EMPTY); }
      const base = this.parts.get(rel) || EMPTY;
      if (offset > base.length) return this._err(409, `offset gap (${offset} > ${base.length})`);
      const head = base.subarray(0, offset);
      const merged = new Uint8Array(offset + body.length);
      merged.set(head, 0);
      merged.set(body, offset);
      this.parts.set(rel, merged);
      this._ensureDirs(rel, true);
      return this._json(200, { ok: true, size: merged.length });
    }

    if (path === '/upload/finalize') {
      const part = this.parts.get(rel);
      if (!part) return this._err(404, 'no part');
      this.parts.delete(rel);
      const name = this._uniqueName(rel);
      const dir = parentOf(rel);
      const dest = dir ? dir + '/' + name : name;
      this.files.set(dest, part);
      this._ensureDirs(dest, true);
      return this._json(200, { ok: true, name, size: part.length });
    }

    if (path === '/upload/abort') {
      this.parts.delete(rel);
      return this._json(200, { ok: true });
    }

    if (path === '/upload/mkdir') {
      this._ensureDirs(rel, false);
      this.dirs.add(rel);
      return this._json(200, { ok: true });
    }

    return this._err(404, 'not found');
  }

  // HttpSink 는 전역 fetch 를 쓴다 → 테스트에서 globalThis.fetch 로 꽂아 준다.
  install() {
    const prev = globalThis.fetch;
    globalThis.fetch = (input, init) => this.fetch(input, init);
    return () => { globalThis.fetch = prev; };
  }
}
