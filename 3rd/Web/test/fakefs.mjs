// 3rd/Web/test/fakefs.mjs
//
// 브라우저 File System Access API 의 최소 부분집합을 메모리로 구현한다.
// Tunnel 이 실제로 사용하는 경로만 지원한다:
//   dir: getDirectoryHandle / getFileHandle / removeEntry
//   file: getFile / createWritable({keepExistingData}) / move
//   writable: write / seek / truncate / close
//
// 이를 통해 Node 에서 저장 폴더 수신(폴더 구조/이어받기/rename) 을 그대로 검증한다.

class MemDir { constructor(name) { this.name = name; this.entries = new Map(); } }
class MemFile { constructor(name, data) { this.name = name; this.data = data || new Uint8Array(0); } }

function notFound() { const e = new Error('not found'); e.name = 'NotFoundError'; throw e; }
function isDir(v) { return v instanceof MemDir; }

class MemFileHandle {
  constructor(file, dir) { this.kind = 'file'; this.name = file.name; this.file = file; this.dir = dir; }
  async getFile() {
    const data = this.file.data;
    return {
      size: data.length,
      async arrayBuffer() { return data.slice().buffer; },
      slice(start, end) { return { arrayBuffer: async () => data.slice(start, end).buffer }; },
      stream() {
        return new ReadableStream({
          start(c) { c.enqueue(data); c.close(); },
        });
      },
    };
  }
  async createWritable(opts) { return new MemWritable(this.file, !!(opts && opts.keepExistingData)); }
  async move(newName) {
    this.dir.entries.delete(this.file.name);
    this.file.name = newName;
    this.dir.entries.set(newName, this.file);
    this.name = newName;
  }
}

class MemWritable {
  constructor(file, keep) {
    this.file = file;
    this.buf = keep ? file.data.slice() : new Uint8Array(0);
    this.pos = 0;
    this.closed = false;
  }
  _ensure(n) {
    if (this.buf.length >= n) return;
    const next = new Uint8Array(n);
    next.set(this.buf);
    this.buf = next;
  }
  async write(chunk) {
    const bytes = chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk);
    this._ensure(this.pos + bytes.length);
    this.buf.set(bytes, this.pos);
    this.pos += bytes.length;
  }
  async seek(p) { this.pos = p; }
  async truncate(size) { this.buf = this.buf.slice(0, size); if (this.pos > size) this.pos = size; }
  async close() { this.file.data = this.buf; this.closed = true; }
}

class MemDirHandle {
  constructor(dir) { this.kind = 'directory'; this.name = dir.name; this.dir = dir; }
  async getDirectoryHandle(name, opts) {
    let e = this.dir.entries.get(name);
    if (e === undefined) {
      if (!opts || !opts.create) notFound();
      e = new MemDir(name); this.dir.entries.set(name, e);
    }
    if (!isDir(e)) { const x = new Error('type mismatch'); x.name = 'TypeMismatchError'; throw x; }
    return new MemDirHandle(e);
  }
  async getFileHandle(name, opts) {
    let e = this.dir.entries.get(name);
    if (e === undefined) {
      if (!opts || !opts.create) notFound();
      e = new MemFile(name); this.dir.entries.set(name, e);
    }
    if (isDir(e)) { const x = new Error('type mismatch'); x.name = 'TypeMismatchError'; throw x; }
    return new MemFileHandle(e, this.dir);
  }
  async removeEntry(name) {
    if (!this.dir.entries.has(name)) notFound();
    this.dir.entries.delete(name);
  }
}

export class MemFS {
  constructor() { this.root = new MemDir(''); this.rootHandle = new MemDirHandle(this.root); }
  get dirHandle() { return this.rootHandle; }

  _find(rel, forWrite) {
    const parts = rel.split('/').filter(Boolean);
    let d = this.root;
    for (let i = 0; i < parts.length - 1; i++) {
      const next = d.entries.get(parts[i]);
      if (!isDir(next)) { if (forWrite) { const nd = new MemDir(parts[i]); d.entries.set(parts[i], nd); d = nd; continue; } return null; }
      d = next;
    }
    return { dir: d, name: parts[parts.length - 1] };
  }
  has(rel) { const f = this._find(rel, false); return !!(f && f.dir.entries.has(f.name)); }
  readBytes(rel) {
    const f = this._find(rel, false);
    if (!f) return null;
    const e = f.dir.entries.get(f.name);
    if (!(e instanceof MemFile)) return null;
    return e.data.slice();
  }
  writeBytes(rel, bytes) {
    const f = this._find(rel, true);
    f.dir.entries.set(f.name, new MemFile(f.name, bytes.slice()));
  }
  isDir(rel) { const f = this._find(rel, false); return !!(f && isDir(f.dir.entries.get(f.name))); }
  list() {
    const out = [];
    const walk = (dir, prefix) => {
      for (const [name, e] of dir.entries) {
        const p = prefix ? prefix + '/' + name : name;
        if (isDir(e)) walk(e, p); else out.push(p);
      }
    };
    walk(this.root, '');
    return out.sort();
  }
}
