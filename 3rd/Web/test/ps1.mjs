// 3rd/Web/test/ps1.mjs
//
// serve.ps1 / combine.ps1 을 실제로 띄워서 /upload API 계약과 페이지 전송을 검증한다.
// (브라우저 없이 HTTP 만 확인 — Web Serial 부분은 test/loopback.mjs / test/uismoke.mjs 담당)
//
//   node 3rd/Web/test/ps1.mjs               (VERBOSE=1 로그, PWSH=<경로> 로 실행기 지정)
//   pwsh 가 없으면 SKIP 하고 exit 0 으로 끝난다.
//
// 검증 항목
//   serve.ps1   : 스크립트 폴더의 transfer.html 을 그대로 서빙, 수신은 -SaveDir 에 기록
//   combine.ps1 : transfer.html 이 없는 폴더에 단독으로 놓아도 내장 페이지를 꺼내 서빙
//   공통        : config / status / part / chunk / finalize(중복 이름) / mkdir / abort,
//                 offset 구멍(409), 저장 폴더 밖 경로(400), 없는 파일(404)

import { spawn, spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { copyFile, mkdir, mkdtemp, readFile, rm } from 'node:fs/promises';
import net from 'node:net';
import { homedir, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert';

const HERE = dirname(fileURLToPath(import.meta.url));
const WEB = join(HERE, '..');
const V = process.env.VERBOSE === '1';
const log = (m) => { if (V) console.log(m); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- pwsh 찾기 ----
function findPwsh() {
  const cands = [
    process.env.PWSH,
    join(homedir(), '.pwsh', 'pwsh'),
    '/usr/bin/pwsh',
    '/usr/local/bin/pwsh',
    '/opt/microsoft/powershell/7/pwsh',
    'C:\\Program Files\\PowerShell\\7\\pwsh.exe',
  ];
  for (const c of cands) if (c && existsSync(c)) return c;
  if (spawnSync('pwsh', ['-NoLogo', '-Command', 'exit 0']).status === 0) return 'pwsh';
  return null;
}

async function freePort() {
  const srv = net.createServer();
  await new Promise((res) => srv.listen(0, '127.0.0.1', res));
  const { port } = srv.address();
  await new Promise((res) => srv.close(res));
  return port;
}

async function startServer(pwsh, script, extra, cwd) {
  const port = await freePort();
  const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', script, '-Port', String(port), '-NoBrowser', ...extra];
  log(`$ ${pwsh} ${args.join(' ')}   (cwd=${cwd})`);
  const child = spawn(pwsh, args, { cwd, stdio: ['ignore', 'pipe', 'pipe'] });
  let out = '';
  child.stdout.on('data', (d) => { out += d; });
  child.stderr.on('data', (d) => { out += d; });
  const url = `http://127.0.0.1:${port}`;

  for (let i = 0; i < 160; i++) {
    if (child.exitCode !== null) throw new Error(`서버가 시작하지 못했습니다(exit ${child.exitCode})\n${out}`);
    try {
      const r = await fetch(url + '/upload/config');
      if (r.ok) { log(out); return { child, url, port, out: () => out }; }
    } catch { /* 아직 안 떴다 */ }
    await sleep(250);
  }
  child.kill('SIGKILL');
  throw new Error(`서버 응답 없음\n${out}`);
}

async function stopServer(s) {
  if (!s) return;
  try { s.child.kill('SIGKILL'); } catch { }
  await sleep(300);
}

const req = (url, path, init) => fetch(url + path, init);
const jget = async (url, path) => (await req(url, path)).json();
const post = (url, path, body) => req(url, path, { method: 'POST', body: body ?? '' });

// ---- 공통 계약 검사 ----
async function checkContract(url, saveDir, label) {
  const cfg = await jget(url, '/upload/config');
  assert.equal(cfg.ok, true, `${label}: config.ok`);
  assert.equal(cfg.root, saveDir, `${label}: config.root`);
  log(`${label}: config root=${cfg.root}`);

  // 페이지: 원본 transfer.html 과 바이트 단위로 같아야 한다.
  const pageRes = await req(url, '/');
  assert.equal(pageRes.status, 200, `${label}: GET /`);
  const served = Buffer.from(await pageRes.arrayBuffer());
  const src = await readFile(join(WEB, 'transfer.html'));
  assert.ok(served.equals(src), `${label}: 페이지가 transfer.html 과 다릅니다 (${served.length} vs ${src.length})`);

  // 1KB 업로드 → 이어받기 정보 → 앞부분 원본
  const bytes = Buffer.alloc(1024);
  for (let i = 0; i < bytes.length; i++) bytes[i] = (i * 7 + 3) & 0xff;

  let r = await post(url, '/upload?path=sub/dir/file.bin&offset=0', bytes);
  assert.equal(r.status, 200, `${label}: chunk`);
  assert.deepEqual(await r.json(), { ok: true, size: 1024 });

  assert.equal((await jget(url, '/upload/status?path=sub/dir/file.bin')).part, 1024, `${label}: status`);
  const partRes = await req(url, '/upload/part?path=sub/dir/file.bin&max=10');
  assert.ok(Buffer.from(await partRes.arrayBuffer()).equals(bytes.subarray(0, 10)), `${label}: part`);

  // offset 구멍은 409
  assert.equal((await post(url, '/upload?path=gap.bin&offset=5', 'x')).status, 409, `${label}: gap 409`);

  // 저장 폴더 밖은 400
  assert.equal((await post(url, '/upload?path=../evil.txt&offset=0', 'x')).status, 400, `${label}: traversal 400`);

  // finalize → 실제 파일
  r = await post(url, '/upload/finalize?path=sub/dir/file.bin');
  assert.deepEqual(await r.json(), { ok: true, name: 'file.bin', size: 1024 }, `${label}: finalize`);
  const saved = await readFile(join(saveDir, 'sub', 'dir', 'file.bin'));
  assert.ok(saved.equals(bytes), `${label}: 저장된 바이트`);

  // 같은 이름을 또 보내면 -1 이 붙는다
  await post(url, '/upload?path=sub/dir/file.bin&offset=0', 'again');
  r = await post(url, '/upload/finalize?path=sub/dir/file.bin');
  assert.equal((await r.json()).name, 'file-1.bin', `${label}: 중복 이름`);

  // mkdir / abort
  assert.equal((await post(url, '/upload/mkdir?path=deep/a/b')).status, 200, `${label}: mkdir`);
  assert.ok(existsSync(join(saveDir, 'deep', 'a', 'b')), `${label}: mkdir 실제 폴더`);

  await post(url, '/upload?path=tmp.bin&offset=0', 'zzz');
  assert.equal((await post(url, '/upload/abort?path=tmp.bin')).status, 200, `${label}: abort`);
  assert.equal(existsSync(join(saveDir, 'tmp.bin.part')), false, `${label}: abort 후 .part 삭제`);

  assert.equal((await req(url, '/no-such-file.html')).status, 404, `${label}: 404`);
  console.log(`  ${label} OK`);
}

// ---- ① serve.ps1 : 스크립트 폴더를 문서 루트로 ----
async function testServe(pwsh) {
  const work = await mkdtemp(join(tmpdir(), 'deskhop-serve-'));
  const save = join(work, 'save');
  const script = join(work, 'serve.ps1');
  await copyFile(join(WEB, 'serve.ps1'), script);
  await copyFile(join(WEB, 'transfer.html'), join(work, 'transfer.html'));
  await mkdir(save, { recursive: true });

  let s;
  try {
    s = await startServer(pwsh, script, ['-SaveDir', save], work);
    await checkContract(s.url, save, 'serve.ps1');
  } finally {
    await stopServer(s);
    await rm(work, { recursive: true, force: true });
  }
}

// ---- ② combine.ps1 : 페이지를 내장한 단일 파일, 옆에 transfer.html 없음 ----
async function testCombine(pwsh) {
  const work = await mkdtemp(join(tmpdir(), 'deskhop-combine-'));
  const runDir = join(work, 'run');          // 엉뚱한 작업 폴더에서 실행해도 동작해야 한다
  const save = join(work, 'save');
  const script = join(work, 'combine.ps1');
  await copyFile(join(WEB, 'combine.ps1'), script);
  await mkdir(runDir, { recursive: true });
  await mkdir(save, { recursive: true });

  let s;
  try {
    s = await startServer(pwsh, script, ['-SaveDir', save], runDir);
    await checkContract(s.url, save, 'combine.ps1');
  } finally {
    await stopServer(s);
  }

  // -SaveDir 를 안 주면 스크립트가 있는 폴더에 저장된다.
  const s2 = await startServer(pwsh, script, [], runDir);
  try {
    const cfg = await jget(s2.url, '/upload/config');
    assert.equal(cfg.root, work, `combine.ps1: 기본 저장 위치는 스크립트 폴더 (${cfg.root})`);
    await post(s2.url, '/upload?path=default.txt&offset=0', 'default-here');
    await post(s2.url, '/upload/finalize?path=default.txt');
    assert.equal(await readFile(join(work, 'default.txt'), 'utf8'), 'default-here', 'combine.ps1: 기본 폴더 저장');
    console.log('  combine.ps1 (기본 저장 위치) OK');
  } finally {
    await stopServer(s2);
    await rm(work, { recursive: true, force: true });
  }
}

async function main() {
  // combine.ps1 이 serve.ps1/transfer.html 과 어긋나 있으면 여기서 바로 잡힌다.
  const chk = spawnSync(process.execPath, [join(WEB, 'build-combined.mjs'), '--check'], { encoding: 'utf8' });
  if (chk.status !== 0) {
    throw new Error(`combine.ps1 이 오래되었습니다\n${chk.stdout}${chk.stderr}`);
  }

  const pwsh = findPwsh();
  if (!pwsh) {
    console.log('SKIP: pwsh/powershell 을 찾지 못했습니다 (PWSH=<경로> 로 지정할 수 있습니다)');
    return;
  }
  console.log(`pwsh: ${pwsh}`);
  await testServe(pwsh);
  await testCombine(pwsh);
  console.log('PS1 SERVER TESTS PASSED');
}

await main();
