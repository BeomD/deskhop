// Web/test/loopback.mjs
//
// Web Serial 없이 두 Tunnel 을 메모리 파이프로 연결해 프로토콜 포팅을 검증한다.
// 수신은 인메모리 가상 폴더(fakefs)로 기록하므로 실제 저장 경로(폴더 구조/
// 이어받기/rename)를 그대로 검증한다.
//
//   node Web/test/loopback.mjs          (VERBOSE=1 로그)

import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import assert from 'node:assert';
import { fileURLToPath } from 'node:url';
import { MemFS } from './fakefs.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// ---- app.js 로드 + 심볼 내보내기 ----
const src = fs.readFileSync(path.join(__dirname, '..', 'app.js'), 'utf8')
  + '\n;globalThis.__DH = { Tunnel, encodeFrame, crc32Of };';
vm.runInThisContext(src, { filename: 'app.js' });
const { Tunnel, encodeFrame, crc32Of } = globalThis.__DH;

const V = process.env.VERBOSE === '1';
const log = (who, m) => { if (V) console.log(`[${who}] ${m}`); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function mkBytes(n, seed) { const b = new Uint8Array(n); for (let i = 0; i < n; i++) b[i] = (i * 31 + seed) & 0xff; return b; }
const eqBytes = (a, b) => a.length === b.length && a.every((v, i) => v === b[i]);

class FakeFile {
  constructor(name, bytes) { this.name = name; this._bytes = bytes; this.size = bytes.length; }
  slice(start, end) { const copy = this._bytes.subarray(start, end).slice(); return { arrayBuffer: async () => copy.buffer }; }
}

// 메모리 듀플렉스 시리얼. 실제 Serial.readLoop 처럼 onBytes 를 직렬 처리한다
// (동시 전달로 인한 파서 재진입을 막는다).
class FakeSerial {
  constructor(name) { this.name = name; this.onBytes = null; this.onError = null; this.peer = null; this.inbox = []; this.draining = false; }
  async write(bytes) { if (this.peer) this.peer._enqueue(bytes.slice()); }
  _enqueue(b) { this.inbox.push(b); if (!this.draining) this._drain(); }
  async _drain() {
    this.draining = true;
    while (this.inbox.length) {
      const b = this.inbox.shift();
      try { if (this.onBytes) await this.onBytes(b); } catch (e) { if (this.onError) this.onError(e); }
    }
    this.draining = false;
  }
  async readLoop() { }
  async close() { this.onBytes = null; this.peer = null; }
}

function pair(nameA, nameB) {
  const a = new FakeSerial(nameA), b = new FakeSerial(nameB);
  a.peer = b; b.peer = a;
  return [a, b];
}

function newTunnel(serial, dirHandle) {
  const t = new Tunnel(serial, { dirHandle, onLog: (s) => log(serial.name, s), onRecvProgress: () => { } });
  t.start();
  clearInterval(t.rateTimer);
  return t;
}

async function connectPair(a, b, dirA, dirB) {
  const A = newTunnel(a, dirA), B = newTunnel(b, dirB);
  await Promise.all([A.connect(), B.connect()]);
  return [A, B];
}

async function testFrameCodec() {
  const payload = new Uint8Array([1, 2, 3, 4]);
  const f = encodeFrame(0x10, payload);
  assert.strictEqual(f.length, 8 + 4 + 4);
  assert.strictEqual(f[0], 0x44); assert.strictEqual(f[1], 0x48); assert.strictEqual(f[2], 0x10);
  const dv = new DataView(f.buffer);
  assert.strictEqual(dv.getUint32(4, true), 4);
  assert.strictEqual(dv.getUint32(12, true), crc32Of(f, 2, 12));
  assert.strictEqual(crc32Of(new TextEncoder().encode('123456789')) >>> 0, 0xCBF43926);
  log('main', 'frame codec OK');
}

async function testLateJoinHandshake() {
  const [a, b] = pair('A', 'B');
  const A = newTunnel(a, null), B = newTunnel(b, null);
  const pa = A.connect();
  await sleep(300);
  const pb = B.connect();
  await Promise.all([pa, pb]);
  await A.close(); await B.close();
  log('main', 'late-join handshake OK');
}

async function testSingleFile() {
  const [a, b] = pair('A', 'B');
  const fsB = new MemFS();
  const [A, B] = await connectPair(a, b, null, fsB.dirHandle);

  const data = mkBytes(100000, 7);
  const file = new FakeFile('hello.bin', data);
  let last = null;
  await A.sendPath({ kind: 'file', file, label: 'hello.bin' }, null, 0, 0, (p) => { last = p; });
  await sleep(60);

  assert.ok(fsB.has('hello.bin'), '파일 저장됨');
  assert.ok(!fsB.has('hello.bin.part'), '.part 제거됨');
  assert.ok(eqBytes(fsB.readBytes('hello.bin'), data), '바이트 일치');
  assert.ok(last && last.complete, '진행률 완료');
  await A.close(); await B.close();
  log('main', 'single file OK (' + data.length + ' bytes)');
}

async function testFolder() {
  const [a, b] = pair('A', 'B');
  const fsB = new MemFS();
  const [A, B] = await connectPair(a, b, null, fsB.dirHandle);

  const f1 = new FakeFile('x', mkBytes(5000, 1)); f1.webkitRelativePath = 'd/a.txt';
  const f2 = new FakeFile('y', mkBytes(12345, 2)); f2.webkitRelativePath = 'd/sub/b.txt';
  await A.sendPath({ kind: 'dir', files: [f1, f2], name: 'd', label: 'd/' }, null, 0, 0, () => { });
  await sleep(60);

  assert.deepStrictEqual(fsB.list(), ['d/a.txt', 'd/sub/b.txt']);
  assert.ok(eqBytes(fsB.readBytes('d/a.txt'), mkBytes(5000, 1)));
  assert.ok(eqBytes(fsB.readBytes('d/sub/b.txt'), mkBytes(12345, 2)));
  await A.close(); await B.close();
  log('main', 'folder OK');
}

async function testEmptyFile() {
  const [a, b] = pair('A', 'B');
  const fsB = new MemFS();
  const [A, B] = await connectPair(a, b, null, fsB.dirHandle);
  await A.sendPath({ kind: 'file', file: new FakeFile('empty.bin', new Uint8Array(0)), label: 'empty.bin' }, null, 0, 0, () => { });
  await sleep(60);
  assert.ok(fsB.has('empty.bin'));
  assert.strictEqual(fsB.readBytes('empty.bin').length, 0);
  await A.close(); await B.close();
  log('main', 'empty file OK');
}

async function testResume() {
  const [a, b] = pair('A', 'B');
  const fsB = new MemFS();
  const [A, B] = await connectPair(a, b, null, fsB.dirHandle);

  const data = mkBytes(30000, 5);
  // 수신측에 정렬된 prefix(8192)만 있는 .part 를 미리 둔다 → 이어받기 유도
  fsB.writeBytes('big.bin.part', data.slice(0, 8192));

  let resumed = false;
  await A.sendPath({ kind: 'file', file: new FakeFile('big.bin', data), label: 'big.bin' }, null, 0, 0, (p) => { if (p.resumed) resumed = true; });
  await sleep(60);

  assert.ok(resumed, '이어받기(resume) 사용됨');
  assert.ok(!fsB.has('big.bin.part'), '.part 제거됨');
  assert.ok(eqBytes(fsB.readBytes('big.bin'), data), '이어받은 내용 일치');
  await A.close(); await B.close();
  log('main', 'resume OK');
}

async function testResumeMismatchRestarts() {
  const [a, b] = pair('A', 'B');
  const fsB = new MemFS();
  const [A, B] = await connectPair(a, b, null, fsB.dirHandle);

  const data = mkBytes(30000, 9);
  // 내용이 다른 prefix 를 두면 검증 실패 → 폐기 후 처음부터
  fsB.writeBytes('big2.bin.part', mkBytes(8192, 123));

  let resumed = false;
  await A.sendPath({ kind: 'file', file: new FakeFile('big2.bin', data), label: 'big2.bin' }, null, 0, 0, (p) => { if (p.resumed) resumed = true; });
  await sleep(60);

  assert.ok(!resumed, '이어받기 사용 안 함');
  assert.ok(eqBytes(fsB.readBytes('big2.bin'), data), '처음부터 정상 수신');
  await A.close(); await B.close();
  log('main', 'resume mismatch → restart OK');
}

async function main() {
  await testFrameCodec();
  await testLateJoinHandshake();
  await testSingleFile();
  await testFolder();
  await testEmptyFile();
  await testResume();
  await testResumeMismatchRestarts();
  console.log('ALL TESTS PASSED');
}

main().catch((e) => { console.error('TEST FAILED:', e); process.exit(1); });
