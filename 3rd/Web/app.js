'use strict';

/* =============================================================================
 *  DeskHop CDC 파일 터널 — 웹(Web Serial) 구현
 *
 *  Go(Transfer/tunnel.go)의 프로토콜을 브라우저로 그대로 포팅한 것입니다.
 *  펌웨어는 pure byte pipe 이므로, 프레임/파일 프로토콜만 맞추면 동일하게 동작합니다.
 *
 *      PC A <--USB CDC--> Pico A <--UART(ARQ)--> Pico B <--USB CDC--> PC B
 *
 *  앱 프레임: "DH" | type(1) | flags(1) | len(4 LE) | payload | crc32(4 LE)
 *  crc32 는 type..payload 전체에 대해 계산합니다.
 *
 *  전송: [JOB_BEGIN] → (FILE_META → CHUNK×N → FILE_DONE) × 항목수 → [JOB_END]
 *        매 FILE_META/CHUNK/FILE_DONE 마다 상대가 ACK 로 흐름 제어.
 *  이어받기: 수신측 .part 의 prefix CRC 를 META_ACK 로 제안 → 송신측이 검증 후 재개.
 * ============================================================================= */

// ============================== CRC32 (IEEE) ==============================
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = (c & 1) ? (0xEDB88320 ^ (c >>> 1)) : (c >>> 1);
    t[n] = c >>> 0;
  }
  return t;
})();

// Go 의 hash/crc32 (IEEE) 와 동일한 의미: crc 는 누적값(최종 XOR 반영 상태).
function crc32Update(crc, buf, start = 0, end = buf.length) {
  crc = (crc ^ 0xFFFFFFFF) >>> 0;
  for (let i = start; i < end; i++) crc = (CRC_TABLE[(crc ^ buf[i]) & 0xFF] ^ (crc >>> 8)) >>> 0;
  return (crc ^ 0xFFFFFFFF) >>> 0;
}
function crc32Of(buf, start = 0, end = buf.length) { return crc32Update(0, buf, start, end); }

// ============================== 바이트 헬퍼 ==============================
function u16le(v) { const b = new Uint8Array(2); new DataView(b.buffer).setUint16(0, v, true); return b; }
function u32le(v) { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, v >>> 0, true); return b; }
function u64le(v) { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(v), true); return b; }
function rdU16(b, o) { return new DataView(b.buffer, b.byteOffset, b.byteLength).getUint16(o, true); }
function rdU32(b, o) { return new DataView(b.buffer, b.byteOffset, b.byteLength).getUint32(o, true); }
function rdU64(b, o) { return Number(new DataView(b.buffer, b.byteOffset, b.byteLength).getBigUint64(o, true)); }
function putU16(b, o, v) { new DataView(b.buffer, b.byteOffset, b.byteLength).setUint16(o, v, true); }
function putU64(b, o, v) { new DataView(b.buffer, b.byteOffset, b.byteLength).setBigUint64(o, BigInt(v), true); }

function concatBytes(a, b) {
  if (!a || a.length === 0) return b;
  const r = new Uint8Array(a.length + b.length);
  r.set(a); r.set(b, a.length);
  return r;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const enc = new TextEncoder();
const dec = new TextDecoder();

// ============================== 프로토콜 상수 ==============================
const MAGIC0 = 0x44; // 'D'
const MAGIC1 = 0x48; // 'H'

const T = {
  HELLO: 0x01, HELLO_ACK: 0x02,
  FILE_META: 0x10, META_ACK: 0x11,
  CHUNK: 0x12, CHUNK_ACK: 0x13,
  FILE_DONE: 0x14, DONE_ACK: 0x15,
  ABORT: 0x16, JOB_BEGIN: 0x17, JOB_END: 0x18,
};

const FLAG_IS_DIR = 0x01;
const META_READY = 0, META_REJECT = 1, META_RESUME = 2;
const ABORT_KEEP = 0, ABORT_DISCARD = 1;
const ROLE_UNKNOWN = 0xFF, ROLE_A = 0, ROLE_B = 1;

const HEADER = 8;      // magic2 + type + flags + len4
const CRCSZ = 4;
const CHUNK_SIZE = 4096;
const WINDOW = 16;
const ACK_EVERY = 4;
const MAX_PAYLOAD = 1 << 20;

const META_RETRY_N = 8, META_RETRY_WAIT = 500;
const DONE_RETRY_N = 10, DONE_RETRY_WAIT = 500;
const CHUNK_ACK_WAIT = 400;

const ERR_TIMEOUT = 'timeout', ERR_CANCELLED = 'cancelled', ERR_CLOSED = 'closed';
function mkErr(kind, msg) { const e = new Error(msg || kind); e.kind = kind; return e; }

function roleToByte(r) { return r === 'A' ? ROLE_A : r === 'B' ? ROLE_B : ROLE_UNKNOWN; }
function roleFromByte(b) { return b === ROLE_A ? 'A' : b === ROLE_B ? 'B' : ''; }
function sideName(r) { return r === 'A' ? 'A' : r === 'B' ? 'B' : '(미상)'; }

function encodeFrame(type, payload) {
  payload = payload || new Uint8Array(0);
  const buf = new Uint8Array(HEADER + payload.length + CRCSZ);
  buf[0] = MAGIC0; buf[1] = MAGIC1; buf[2] = type; buf[3] = 0;
  new DataView(buf.buffer).setUint32(4, payload.length, true);
  buf.set(payload, HEADER);
  new DataView(buf.buffer).setUint32(HEADER + payload.length, crc32Of(buf, 2, HEADER + payload.length), true);
  return buf;
}

function safeRelPath(p) {
  p = p.replace(/\\/g, '/');
  if (p.startsWith('/')) return null;
  if (p.length >= 2 && p[1] === ':') return null;
  const out = [];
  for (const part of p.split('/')) {
    if (part === '' || part === '.') continue;
    if (part === '..') return null;
    out.push(part);
  }
  return out.length ? out.join('/') : null;
}
function relBase(rel) { const i = rel.lastIndexOf('/'); return i < 0 ? rel : rel.slice(i + 1); }
function relDir(rel) { const i = rel.lastIndexOf('/'); return i < 0 ? '' : rel.slice(0, i); }

// ============================== 동기화 프리미티브 ==============================
// await 가능한 채널. push 는 즉시, pop 은 timeout/signal 지원.
class Chan {
  constructor() { this.items = []; this.waiters = []; }
  push(v) { if (this.waiters.length) this.waiters.shift()(v); else this.items.push(v); }
  drain() { this.items.length = 0; }
  pop(timeoutMs, signal) {
    if (this.items.length) return Promise.resolve(this.items.shift());
    return new Promise((resolve, reject) => {
      let done = false;
      let timer = null;
      const cleanup = () => {
        if (timer) clearTimeout(timer);
        if (signal) signal.removeEventListener('abort', onAbort);
        const i = this.waiters.indexOf(w);
        if (i >= 0) this.waiters.splice(i, 1);
      };
      const w = (v) => { if (done) return; done = true; cleanup(); resolve(v); };
      const onAbort = () => { if (done) return; done = true; cleanup(); reject(mkErr(ERR_CANCELLED, '연결이 닫혔거나 취소되었습니다')); };
      if (timeoutMs > 0) timer = setTimeout(() => { if (done) return; done = true; cleanup(); reject(mkErr(ERR_TIMEOUT, '응답 타임아웃')); }, timeoutMs);
      if (signal) {
        if (signal.aborted) { onAbort(); return; }
        signal.addEventListener('abort', onAbort);
      }
      this.waiters.push(w);
    });
  }
}

// 경계가 있는 큐(수신 프레임 백프레셔).
class BoundedQueue {
  constructor(max) { this.max = max; this.items = []; this.popWaiters = []; this.pushWaiters = []; }
  async push(v) {
    while (this.items.length >= this.max) await new Promise((r) => this.pushWaiters.push(r));
    this.items.push(v);
    if (this.popWaiters.length) this.popWaiters.shift()();
  }
  async pop() {
    while (this.items.length === 0) await new Promise((r) => this.popWaiters.push(r));
    const v = this.items.shift();
    if (this.pushWaiters.length) this.pushWaiters.shift()();
    return v;
  }
  reset() { this.items.length = 0; while (this.pushWaiters.length) this.pushWaiters.shift()(); }
}

// ============================== 시리얼 래퍼 ==============================
class Serial {
  constructor(port) {
    this.port = port;
    this.writer = null; this.reader = null;
    this.writeChain = Promise.resolve();
    this.onBytes = null; this.onError = null;
    this.running = false;
  }
  async open(baud) {
    await this.port.open({ baudRate: baud || 6000000 });
    this.writer = this.port.writable.getWriter();
    this.reader = this.port.readable.getReader();
  }
  async resetDTR() {
    // 펌웨어가 자기 ARQ 상태를 리셋하도록 DTR 을 내렸다 올린다.
    try { await this.port.setSignals({ dataTerminalReady: false }); } catch (e) { /* 일부 스택은 미지원 */ }
    await sleep(150);
    try { await this.port.setSignals({ dataTerminalReady: true }); } catch (e) { }
    await sleep(150);
  }
  write(bytes) {
    this.writeChain = this.writeChain
      .then(() => this.writer.write(bytes))
      .catch((e) => { if (this.onError) this.onError(e); });
    return this.writeChain;
  }
  async readLoop() {
    this.running = true;
    while (this.running) {
      let res;
      try { res = await this.reader.read(); } catch (e) { break; }
      if (res.done) break;
      if (res.value && res.value.length && this.onBytes) {
        try { await this.onBytes(res.value); } catch (e) { /* 백프레셔/파싱 오류 */ }
      }
    }
    this.running = false;
  }
  async close() {
    this.running = false;
    try { this.reader.cancel(); } catch (e) { }
    try { this.reader.releaseLock(); } catch (e) { }
    try { this.writer.releaseLock(); } catch (e) { }
    try { await this.port.close(); } catch (e) { }
  }
}

// ============================== 터널 로직 ==============================
class Tunnel {
  constructor(serial, opts = {}) {
    this.s = serial;
    this.dirHandle = opts.dirHandle || null;       // 저장 폴더(File System Access)
    this.onLog = opts.onLog || (() => { });
    this.onRecvProgress = opts.onRecvProgress || (() => { });

    this.acc = new Uint8Array(0);
    this.bannerDone = false;
    this.localRole = ''; this.peerRole = '';
    this.closed = false;
    this.closeCtrl = new AbortController();

    this.helloSeen = false; this.helloWaiters = [];
    this.metaAck = new Chan();
    this.doneAck = new Chan();
    this.acks = new Chan();
    this.incoming = new BoundedQueue(128);

    this.bytesRead = 0; this.bytesWritten = 0;

    // 수신 상태
    this.cur = null;
    this.recvJob = null;
    this.lastMeta = null;   // { rel, size, ack }
    this.lastDone = null;   // { ok, size, crc }

    this.worker = null;
  }

  log(m) { this.onLog(m); }
  recvProgress(p) { this.onRecvProgress(p); }
  signalFor(user) {
    const a = this.closeCtrl.signal;
    if (!user) return a;
    if (AbortSignal.any) return AbortSignal.any([user, a]);
    const c = new AbortController();
    const on = () => c.abort();
    user.addEventListener('abort', on); a.addEventListener('abort', on);
    if (user.aborted || a.aborted) c.abort();
    return c.signal;
  }

  start() {
    this.s.onBytes = (b) => this.feed(b);
    this.s.onError = (e) => this.fail(e);
    this.s.readLoop();
    this.worker = this.recvWorker();
    this.rateTimer = setInterval(() => this.rateTick(), 1000);
  }

  async fail(err) {
    this.log('터널 오류: ' + (err && err.message ? err.message : err));
    await this.close();
  }

  async close() {
    if (this.closed) return;
    this.closed = true;
    this.closeCtrl.abort();
    clearInterval(this.rateTimer);
    await this.closeCur(false); // 이어받기 위해 .part 보존
    // 수신 워커를 종료시킨다(대기 중이면 깨운다).
    this.incoming.items = [{ type: '__close__' }];
    if (this.incoming.popWaiters.length) this.incoming.popWaiters.shift()();
    await this.s.close();
  }

  rateTick() {
    const r = this.bytesRead, w = this.bytesWritten;
    const dr = r - (this._lastR || 0), dw = w - (this._lastW || 0);
    this._lastR = r; this._lastW = w;
    if (dr || dw) this.log(`CDC 속도: 읽기 ${fmtRate(dr)} · 쓰기 ${fmtRate(dw)}`);
  }

  helloPayload() { return new Uint8Array([roleToByte(this.localRole)]); }
  roles() { return [this.localRole, this.peerRole]; }

  signalHello() { this.helloSeen = true; const w = this.helloWaiters; this.helloWaiters = []; for (const r of w) r(); }
  waitHello(timeoutMs, signal) {
    if (this.helloSeen) return Promise.resolve();
    return new Promise((resolve, reject) => {
      let done = false; let timer = null;
      const cleanup = () => { if (timer) clearTimeout(timer); if (signal) signal.removeEventListener('abort', onAbort); const i = this.helloWaiters.indexOf(w); if (i >= 0) this.helloWaiters.splice(i, 1); };
      const w = () => { if (done) return; done = true; cleanup(); resolve(); };
      const onAbort = () => { if (done) return; done = true; cleanup(); reject(mkErr(ERR_CLOSED, '연결이 닫혔습니다')); };
      if (timeoutMs > 0) timer = setTimeout(() => { if (done) return; done = true; cleanup(); reject(mkErr(ERR_TIMEOUT)); }, timeoutMs);
      if (signal) { if (signal.aborted) { onAbort(); return; } signal.addEventListener('abort', onAbort); }
      this.helloWaiters.push(w);
    });
  }

  // ---- 연결(HELLO 핸드셰이크) ----
  connect() { return this.doConnect(20000); }
  connectWait() { return this.doConnect(0); }
  async doConnect(totalMs) {
    const retry = 400;
    const deadline = totalMs > 0 ? Date.now() + totalMs : 0;
    let first = true;
    for (;;) {
      await this.sendFrame(T.HELLO, this.helloPayload());
      if (first) {
        this.log(totalMs > 0
          ? `HELLO 전송 — 상대편에서도 '연결'을 눌러주세요 (최대 ${Math.round(totalMs / 1000)}초 대기)`
          : 'HELLO 전송 — 상대편이 연결하면 자동으로 이어집니다 (해제할 때까지 대기)');
        first = false;
      }
      try {
        await this.waitHello(retry, this.closeCtrl.signal);
        return;
      } catch (e) {
        if (e.kind === ERR_CLOSED) throw new Error('연결이 닫혔습니다');
        if (e.kind === ERR_TIMEOUT) {
          if (totalMs > 0 && Date.now() > deadline) {
            throw new Error("상대 보드 응답이 없습니다 (HELLO 타임아웃). 양쪽 모두 '연결'을 눌렀는지, 미러 모드가 꺼져 있는지 확인하세요");
          }
          continue;
        }
        throw e;
      }
    }
  }

  // ---- 프레임 송신/수신 ----
  async sendFrame(type, payload) { await this.rawWrite(encodeFrame(type, payload)); }
  async rawWrite(bytes) {
    if (this.closed) throw mkErr(ERR_CLOSED, '연결이 닫혔습니다');
    await this.s.write(bytes);
    this.bytesWritten += bytes.length;
  }

  async feed(chunk) {
    this.bytesRead += chunk.length;
    this.acc = concatBytes(this.acc, chunk);
    if (!this.bannerDone && this.extractBanner()) return; // 배너 일부만 도착: 파서 보류
    await this.parseFrames();
    if (this.acc.length > (1 << 20)) this.acc = new Uint8Array(0);
  }

  // 역할 배너("#ROLE:A\n")를 스트림 맨 앞에서 떼어낸다.
  // 일부만 도착해 개행 전이면 true 를 반환해 호출자가 파싱을 보류하게 한다.
  extractBanner() {
    const prefix = enc.encode('#ROLE:');
    const idx = indexOfBytes(this.acc, prefix);
    if (idx < 0) { if (this.acc.length > 256) this.bannerDone = true; return false; }
    let nl = -1;
    for (let i = idx + prefix.length; i < this.acc.length; i++) { if (this.acc[i] === 0x0A) { nl = i; break; } }
    if (nl < 0) { this.acc = this.acc.subarray(idx); return true; } // 개행 대기
    const role = dec.decode(this.acc.subarray(idx + prefix.length, nl)).trim();
    if (role === 'A' || role === 'B') { this.localRole = role; this.log('역할 배너 수신: 이 창=' + sideName(role)); }
    this.bannerDone = true;
    this.acc = concatBytes(this.acc.subarray(0, idx), this.acc.subarray(nl + 1));
    return false;
  }

  async parseFrames() {
    let acc = this.acc;
    for (;;) {
      let i = -1;
      for (let j = 0; j + 1 < acc.length; j++) { if (acc[j] === MAGIC0 && acc[j + 1] === MAGIC1) { i = j; break; } }
      if (i < 0) {
        acc = (acc.length > 0 && acc[acc.length - 1] === MAGIC0) ? acc.subarray(acc.length - 1) : acc.subarray(0, 0);
        break;
      }
      if (i > 0) acc = acc.subarray(i);
      if (acc.length < HEADER + CRCSZ) break;
      if (acc[1] !== MAGIC1) { acc = acc.subarray(1); continue; }
      const ln = rdU32(acc, 4);
      if (ln > MAX_PAYLOAD) { acc = acc.subarray(1); continue; }
      const total = HEADER + ln + CRCSZ;
      if (acc.length < total) break;
      if (crc32Of(acc, 2, HEADER + ln) !== rdU32(acc, HEADER + ln)) { acc = acc.subarray(1); continue; }
      const type = acc[2];
      const payload = acc.slice(HEADER, HEADER + ln);
      acc = acc.subarray(total);
      await this.dispatch(type, payload);
    }
    this.acc = acc.slice();
  }

  async dispatch(type, payload) {
    switch (type) {
      case T.HELLO_ACK: {
        const r = roleFromByte(payload[0]); if (r) this.peerRole = r;
        this.signalHello();
        break;
      }
      case T.HELLO: {
        const r = roleFromByte(payload[0]); if (r) this.peerRole = r;
        this.signalHello();
        await this.closeCur(false);
        this.recvJob = null;
        await this.sendFrame(T.HELLO_ACK, this.helloPayload());
        break;
      }
      case T.META_ACK: this.metaAck.push(payload); break;
      case T.DONE_ACK: this.doneAck.push(payload); break;
      case T.CHUNK_ACK: if (payload.length >= 4) this.acks.push(rdU32(payload, 0)); break;
      default: await this.incoming.push({ type, payload });
    }
  }

  // ---- 수신 워커 ----
  async recvWorker() {
    for (;;) {
      const f = await this.incoming.pop();
      if (f && f.type === '__close__') return;
      if (this.closed) continue;
      try { await this.handleRecv(f); }
      catch (e) { this.log('수신 처리 오류: ' + (e && e.message ? e.message : e)); }
    }
  }

  async handleRecv(f) {
    switch (f.type) {
      case T.JOB_BEGIN: this.recvJobBegin(f.payload); break;
      case T.JOB_END: this.recvJob = null; break;
      case T.FILE_META: await this.recvMeta(f.payload); break;
      case T.CHUNK: await this.recvChunk(f.payload); break;
      case T.FILE_DONE: await this.recvDone(f.payload); break;
      case T.ABORT: await this.recvAbort(f.payload); break;
    }
  }

  recvJobBegin(payload) {
    if (payload.length < 12) { this.recvJob = { total: 0, totalBytes: 0, done: 0, doneBytes: 0, index: 0 }; return; }
    this.recvJob = {
      total: rdU32(payload, 0), totalBytes: rdU64(payload, 4),
      done: 0, doneBytes: 0, index: 0,
    };
  }

  async recvAbort(payload) {
    const reason = payload.length > 0 ? payload[0] : ABORT_KEEP;
    const discard = reason === ABORT_DISCARD;
    if (this.cur) this.log('수신 중단: ' + this.cur.rel + (discard ? ' (부분 파일 폐기)' : ' (부분 파일 보존)'));
    else this.log('수신 중단 요청을 받았습니다.');
    await this.closeCur(discard);
  }

  async recvMeta(payload) {
    if (payload.length < 3) { await this.sendMetaAck(META_REJECT, 0, 0); return; }
    const flags = payload[0];
    const nameLen = rdU16(payload, 1);
    if (payload.length < 3 + nameLen + 8) { await this.sendMetaAck(META_REJECT, 0, 0); return; }
    const rawName = dec.decode(payload.subarray(3, 3 + nameLen));
    const size = rdU64(payload, 3 + nameLen);
    const isDir = (flags & FLAG_IS_DIR) !== 0;

    const rel = safeRelPath(rawName);
    if (!rel) { this.log('거부된 경로(안전하지 않음): ' + rawName); await this.sendMetaAck(META_REJECT, 0, 0); return; }

    // 같은 항목의 FILE_META 재전송(META_ACK 유실 복구): 마지막 응답을 그대로 되돌린다.
    if (this.cur && this.cur.isDir === isDir && this.cur.rel === rel && this.cur.size === size && this.lastMeta) {
      await this.sendFrame(T.META_ACK, this.lastMeta.ack);
      return;
    }

    await this.closeCur(true); // 새 항목 시작: 이전 중단분 폐기
    if (this.recvJob) this.recvJob.index++;
    const idx = this.recvJob ? this.recvJob.index : 0;

    if (isDir) {
      if (this.dirHandle) {
        try { await this.ensureDir(rel); }
        catch (e) { this.log('폴더 생성 실패: ' + e.message); await this.sendMetaAck(META_REJECT, 0, 0); return; }
      }
      await this.rememberMetaAck(rel, 0, META_READY, 0, 0);
      this.log('폴더 생성: ' + rel);
      this.cur = { isDir: true, rel, size: 0, index: idx };
      return;
    }

    if (!this.dirHandle) {
      this.log('저장 폴더가 없어 수신을 거부합니다: ' + rel);
      await this.sendMetaAck(META_REJECT, 0, 0);
      return;
    }

    const parent = relDir(rel);
    const tmpName = relBase(rel) + '.part';
    let parentDir, tmpHandle;
    try {
      parentDir = await this.resolveDir(parent, true);
      tmpHandle = await parentDir.getFileHandle(tmpName, { create: true });
    } catch (e) { this.log('임시 파일 생성 실패: ' + e.message); await this.sendMetaAck(META_REJECT, 0, 0); return; }

    // 이어받기: 남아 있는 .part 의 정렬된 prefix CRC 를 제안.
    let startOffset = 0, prefixCRC = 0;
    try {
      const f = await tmpHandle.getFile();
      if (f.size > 0 && f.size <= size) {
        const aligned = Math.floor(f.size / CHUNK_SIZE) * CHUNK_SIZE;
        if (aligned > 0) { const c = await crcFilePrefix(f, aligned); if (c !== null) { startOffset = aligned; prefixCRC = c; } }
      }
    } catch (e) { /* 없으면 처음부터 */ }

    const recv = mkRecvFile(rel, size, idx, { parentDir, tmpHandle, tmpName });
    if (startOffset > 0) {
      try {
        const writer = await tmpHandle.createWritable({ keepExistingData: true });
        await writer.truncate(startOffset);
        await writer.seek(startOffset);
        recv.writer = writer; recv.next = startOffset / CHUNK_SIZE; recv.written = startOffset;
        recv.crc = prefixCRC; recv.baseBytes = startOffset;
        await this.rememberMetaAck(rel, size, META_RESUME, startOffset, prefixCRC);
        this.log(`이어받기 제안: ${rel} (offset ${startOffset})`);
        this.cur = recv;
        return;
      } catch (e) { this.log('이어받기 초기화 실패, 처음부터: ' + e.message); }
    }
    try {
      recv.writer = await tmpHandle.createWritable();
    } catch (e) { this.log('파일 열기 실패: ' + e.message); await this.sendMetaAck(META_REJECT, 0, 0); return; }
    await this.rememberMetaAck(rel, size, META_READY, 0, 0);
    this.log(`수신 시작: ${rel} (${size} bytes)`);
    this.cur = recv;
  }

  async recvChunk(payload) {
    const cur = this.cur;
    if (!cur || cur.isDir || payload.length < 4) return;
    const seq = rdU32(payload, 0);
    const data = payload.subarray(4);
    if (seq !== cur.next) { await this.sendFrame(T.CHUNK_ACK, u32le(cur.next)); return; }
    try { await cur.writer.write(data); }
    catch (e) { this.log('파일 쓰기 실패: ' + e.message); return; }
    cur.written += data.length;
    cur.crc = crc32Update(cur.crc, data);
    cur.next++;
    if (cur.next % ACK_EVERY === 0) await this.sendFrame(T.CHUNK_ACK, u32le(cur.next));
    this.emitRecvProgress(cur, false);
  }

  async recvDone(payload) {
    if (payload.length < 12) { await this.closeCur(true); await this.sendDoneAck(1); return; }
    const wantCRC = rdU32(payload, 0);
    const wantSize = rdU64(payload, 4);
    const cur = this.cur;

    if (!cur) {
      if (this.lastDone && this.lastDone.ok && wantSize === this.lastDone.size && wantCRC === this.lastDone.crc) await this.sendDoneAck(0);
      else await this.sendDoneAck(1);
      return;
    }
    if (cur.isDir) {
      this.lastDone = { ok: true, size: 0, crc: 0 };
      this.recvCount(cur);
      this.cur = null;
      await this.sendDoneAck(0);
      return;
    }

    try { await cur.writer.close(); } catch (e) { }

    const ok = cur.written === cur.size && wantSize === cur.size && cur.crc === wantCRC;
    this.lastDone = { ok, size: cur.size, crc: wantCRC };
    if (!ok) {
      this.log(`수신 실패: ${cur.rel} (크기 ${cur.written}/${cur.size}, CRC ${(cur.crc >>> 0).toString(16)}/${(wantCRC >>> 0).toString(16)}) — 부분 파일 폐기`);
      await this.removeTmp(cur);
      this.cur = null;
      await this.sendDoneAck(1);
      return;
    }

    try { await this.commitFile(cur); }
    catch (e) { this.log('저장 실패: ' + e.message); this.lastDone.ok = false; await this.removeTmp(cur); this.cur = null; await this.sendDoneAck(1); return; }
    this.log(`파일 저장 완료: ${cur.rel} (${cur.written} bytes)`);
    this.recvCount(cur);
    this.cur = null;
    await this.sendDoneAck(0);
  }

  recvCount(cur) {
    if (!this.recvJob) return;
    this.recvJob.done++;
    if (!cur.isDir) this.recvJob.doneBytes += cur.size;
    this.recvProgress({
      index: cur.index, total: this.recvJob.total, name: cur.rel,
      done: cur.size, entryBytes: cur.size,
      overallDone: this.recvJob.doneBytes, overallTotal: this.recvJob.totalBytes, complete: true,
    });
  }

  emitRecvProgress(cur, complete) {
    let base = this.recvJob ? this.recvJob.doneBytes : 0;
    const done = base + (cur.isDir ? 0 : cur.written);
    let total = 0, idx = 0, cnt = 0;
    if (this.recvJob) { total = this.recvJob.totalBytes; idx = this.recvJob.index; cnt = this.recvJob.total; }
    else { total = cur.size; idx = 1; cnt = 1; }
    this.recvProgress({ index: idx, total: cnt, name: cur.rel, done: cur.written, entryBytes: cur.size, overallDone: done, overallTotal: total, complete: !!complete });
  }

  async closeCur(discard) {
    const cur = this.cur; if (!cur) return;
    this.cur = null;
    try { if (cur.writer) await cur.writer.close(); } catch (e) { }
    if (discard) await this.removeTmp(cur);
  }
  async removeTmp(cur) {
    if (cur.parentDir && cur.tmpName) { try { await cur.parentDir.removeEntry(cur.tmpName); } catch (e) { } }
  }

  async commitFile(cur) {
    const finalName0 = relBase(cur.rel);
    const finalName = await uniqueNameInDir(cur.parentDir, finalName0);
    if (typeof cur.tmpHandle.move === 'function') { await cur.tmpHandle.move(finalName); return; }
    // move() 미지원: 복사 후 .part 삭제
    const src = await cur.tmpHandle.getFile();
    const dst = await cur.parentDir.getFileHandle(finalName, { create: true });
    const w = await dst.createWritable();
    await src.stream().pipeTo(w);
    await cur.parentDir.removeEntry(cur.tmpName);
  }

  // ---- 저장 폴더 헬퍼 ----
  async resolveDir(rel, create) {
    let d = this.dirHandle;
    if (!rel) return d;
    for (const part of rel.split('/')) d = await d.getDirectoryHandle(part, { create: !!create });
    return d;
  }
  async ensureDir(rel) { if (rel) await this.resolveDir(rel, true); }

  async sendMetaAck(status, offset, prefixCRC) {
    const p = metaAckPayload(status, offset, prefixCRC);
    await this.sendFrame(T.META_ACK, p); await this.sendFrame(T.META_ACK, p); // 첫 프레임 유실 대비
  }
  async rememberMetaAck(rel, size, status, offset, prefixCRC) {
    const ack = metaAckPayload(status, offset, prefixCRC);
    this.lastMeta = { rel, size, ack };
    await this.sendFrame(T.META_ACK, ack); await this.sendFrame(T.META_ACK, ack);
  }
  async sendDoneAck(status) {
    await this.sendFrame(T.DONE_ACK, new Uint8Array([status])); await this.sendFrame(T.DONE_ACK, new Uint8Array([status]));
  }

  // ============================== 송신측 ==============================
  async sendPath(root, userSignal, queueIndex, queueTotal, cb) {
    const entries = collectEntries(root);
    if (!entries.length) throw new Error('전송할 항목이 없습니다');
    const total = entries.length;
    let totalBytes = 0;
    for (const e of entries) totalBytes += e.size;

    const signal = this.signalFor(userSignal);
    this.acks.drain();

    const jb = new Uint8Array(12);
    new DataView(jb.buffer).setUint32(0, total, true);
    putU64(jb, 4, totalBytes);
    await this.sendFrame(T.JOB_BEGIN, jb);

    let overall = 0;
    try {
      for (let i = 0; i < entries.length; i++) {
        if (signal.aborted) { await this.sendFrame(T.ABORT, new Uint8Array([ABORT_KEEP])); throw mkErr(ERR_CANCELLED); }
        const e = entries[i];
        const idx = i + 1;
        const prog = cb ? (sent, complete, resumed) => cb(makeProgress(idx, total, e.rel, sent, e.size, overall + sent, totalBytes, complete, resumed, queueIndex, queueTotal)) : null;
        try {
          await this.sendOne(e, signal, prog);
        } catch (err) {
          await this.sendFrame(T.ABORT, new Uint8Array([ABORT_KEEP]));
          throw err;
        }
        overall += e.size;
      }
      const je = u32le(total);
      await this.sendFrame(T.JOB_END, je);
    } catch (e) { throw e; }
  }

  async sendOne(e, signal, prog) {
    this.metaAck.drain(); this.doneAck.drain();

    if (e.isDir) {
      const meta = buildMeta(e);
      let ack;
      for (let attempt = 0; ; attempt++) {
        await this.sendFrame(T.FILE_META, meta);
        try { ack = await this.metaAck.pop(META_RETRY_WAIT, signal); break; }
        catch (err) {
          if (err.kind === ERR_TIMEOUT && attempt < META_RETRY_N) { this.log(`META_ACK 대기 초과 — 재전송 ${attempt + 1}: ${e.rel}`); continue; }
          throw err;
        }
      }
      if (ack.length >= 1 && ack[0] === META_REJECT) throw new Error('수신측이 거부했습니다: ' + e.rel);
      await this.finishFile(0, 0, signal, prog, false);
      return;
    }

    // FILE_META + 이어받기 협상
    let startOffset = 0, prefixCRC = 0, metaRetries = 0;
    for (let resumeAttempt = 0; ;) {
      await this.sendFrame(T.FILE_META, buildMeta(e));
      let ack;
      try { ack = await this.metaAck.pop(META_RETRY_WAIT, signal); }
      catch (err) {
        if (err.kind === ERR_TIMEOUT && metaRetries < META_RETRY_N) { metaRetries++; this.log(`META_ACK 대기 초과 — 재전송 ${metaRetries}: ${e.rel}`); continue; }
        throw err;
      }
      const status = ack.length >= 1 ? ack[0] : META_REJECT;
      if (status === META_REJECT) throw new Error('수신측이 거부했습니다: ' + e.rel);
      if (status === META_RESUME && ack.length >= 13) {
        let off = Number(rdU64(ack, 1));
        const wantPrefix = rdU32(ack, 9);
        off = Math.floor(off / CHUNK_SIZE) * CHUNK_SIZE;
        const got = off > 0 ? await crcFilePrefix(e.file, off) : null;
        if (got !== null && got === wantPrefix) { startOffset = off; prefixCRC = got; break; }
        if (resumeAttempt >= 1) throw new Error('이어받기 검증 실패 (파일 변경 가능성)');
        resumeAttempt++;
        await this.sendFrame(T.ABORT, new Uint8Array([ABORT_DISCARD]));
        this.metaAck.drain(); // 첫 제안의 잔여(중복) META_ACK 제거
        await sleep(50);
        continue;
      }
      break; // META_READY
    }

    if (startOffset > 0 && prog) prog(startOffset, false, true);

    let seq = startOffset / CHUNK_SIZE;
    let crc = prefixCRC;
    let sent = startOffset;
    const start = Date.now();
    let inflight = [];

    for (; ;) {
      if (signal.aborted) throw mkErr(ERR_CANCELLED);
      if (sent >= e.size) break;
      const end = Math.min(sent + CHUNK_SIZE, e.size);
      const buf = new Uint8Array(await e.file.slice(sent, end).arrayBuffer());
      crc = crc32Update(crc, buf);

      while (inflight.length >= WINDOW) inflight = await this.pumpAcks(inflight, signal);

      const payload = new Uint8Array(4 + buf.length);
      new DataView(payload.buffer).setUint32(0, seq, true);
      payload.set(buf, 4);
      const frame = encodeFrame(T.CHUNK, payload);
      await this.rawWrite(frame);
      inflight.push({ seq, frame });
      seq++;
      sent += buf.length;
      if (prog) prog(sent, false, startOffset > 0);
    }

    await this.finishFile(crc, sent, signal, prog, startOffset > 0);
    const rate = fmtRate(sent - startOffset, Date.now() - start);
    this.log(`전송 완료: ${e.rel} (${sent - startOffset} bytes, ${rate})`);
  }

  async finishFile(crc, size, signal, prog, resumed) {
    const done = new Uint8Array(12);
    new DataView(done.buffer).setUint32(0, crc, true);
    putU64(done, 4, size);
    this.doneAck.drain();

    let lastErr = null;
    for (let attempt = 0; attempt < DONE_RETRY_N; attempt++) {
      await this.sendFrame(T.FILE_DONE, done);
      let ack;
      try { ack = await this.doneAck.pop(DONE_RETRY_WAIT, signal); }
      catch (err) {
        if (err.kind === ERR_TIMEOUT) { lastErr = err; this.log(`DONE_ACK 대기 초과 — 재전송 ${attempt + 1}`); continue; }
        throw err;
      }
      if (ack.length >= 1 && ack[0] !== 0) throw new Error('수신측 검증 실패 (CRC/크기 불일치)');
      if (prog) prog(size, true, resumed);
      return;
    }
    throw lastErr || mkErr(ERR_TIMEOUT);
  }

  async pumpAcks(inflight, signal) {
    try {
      const a = await this.acks.pop(CHUNK_ACK_WAIT, signal);
      while (inflight.length > 0 && inflight[0].seq < a) inflight.shift();
      return inflight;
    } catch (err) {
      if (err.kind === ERR_TIMEOUT) {
        if (inflight.length) await this.rawWrite(inflight[0].frame); // 가장 오래된 미확인 하나만 재전송
        return inflight;
      }
      throw err;
    }
  }
}

// ============================== 프로토콜 보조 ==============================
function metaAckPayload(status, offset, prefixCRC) {
  const p = new Uint8Array(13);
  p[0] = status;
  putU64(p, 1, offset);
  new DataView(p.buffer).setUint32(9, prefixCRC >>> 0, true);
  return p;
}
function buildMeta(e) {
  const name = enc.encode(e.rel);
  const meta = new Uint8Array(1 + 2 + name.length + 8);
  meta[0] = e.isDir ? FLAG_IS_DIR : 0;
  putU16(meta, 1, name.length);
  meta.set(name, 3);
  putU64(meta, 3 + name.length, e.size);
  return meta;
}
function mkRecvFile(rel, size, index, extra) {
  return Object.assign({
    isDir: false, rel, size, index,
    next: 0, written: 0, crc: 0, baseBytes: 0, entrySize: size, start: Date.now(),
  }, extra);
}
function makeProgress(index, total, name, done, entryBytes, overallDone, overallTotal, complete, resumed, queueIndex, queueTotal) {
  return { index, total, name, done, entryBytes, overallDone, overallTotal, complete: !!complete, resumed: !!resumed, queueIndex: queueIndex || 0, queueTotal: queueTotal || 0 };
}

// ============================== 파일 수집 ==============================
function collectEntries(root) {
  if (root.kind === 'file') return [{ rel: root.file.name, file: root.file, size: root.file.size, isDir: false }];
  const dirSet = new Set(); const dirs = []; const files = [];
  for (const f of root.files) {
    const rel = (f.webkitRelativePath || f.name).replace(/\\/g, '/');
    const parts = rel.split('/');
    for (let i = 1; i < parts.length; i++) { const d = parts.slice(0, i).join('/'); if (!dirSet.has(d)) { dirSet.add(d); dirs.push(d); } }
    files.push({ rel, file: f, size: f.size, isDir: false });
  }
  dirs.sort();
  return [...dirs.map((d) => ({ rel: d, isDir: true, size: 0 })), ...files];
}

async function crcFilePrefix(file, n) {
  let crc = 0, off = 0; const S = 1 << 20;
  try {
    while (off < n) {
      const end = Math.min(off + S, n);
      const buf = new Uint8Array(await file.slice(off, end).arrayBuffer());
      crc = crc32Update(crc, buf);
      off = end;
    }
  } catch (e) { return null; }
  return crc >>> 0;
}

async function uniqueNameInDir(dirHandle, name) {
  const dot = name.lastIndexOf('.');
  const base = dot > 0 ? name.slice(0, dot) : name;
  const ext = dot > 0 ? name.slice(dot) : '';
  let cand = name;
  for (let i = 0; i < 10000; i++) {
    if (i > 0) cand = base + '-' + i + ext;
    try { await dirHandle.getFileHandle(cand); }
    catch (e) { if (e.name === 'NotFoundError') return cand; throw e; }
  }
  return base + '-' + Date.now() + ext;
}


function indexOfBytes(hay, needle) {
  outer: for (let i = 0; i <= hay.length - needle.length; i++) {
    for (let j = 0; j < needle.length; j++) if (hay[i + j] !== needle[j]) continue outer;
    return i;
  }
  return -1;
}

function fmtRate(bytes, ms) {
  if (!bytes || !ms) return '0 KB/s';
  const kbs = bytes / 1024 / (ms / 1000);
  return kbs >= 1024 ? (kbs / 1024).toFixed(2) + ' MB/s' : kbs.toFixed(1) + ' KB/s';
}
function humanBytes(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(1) + ' ' + units[i];
}
