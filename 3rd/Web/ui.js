'use strict';

/* =============================================================================
 *  Web UI — Transfer(GUI) 의 DOM 컨트롤러
 *
 *  UI 하위(lib)는 app.js 의 Serial/Tunnel/progress 유틸을 사용하고, 이 파일은
 *  포트 선택, 대기열, 진행률, 저장 폴더, 취소 등 화면 동작만 담당한다.
 * ============================================================================= */

const el = (id) => document.getElementById(id);

// DeskHop 보드의 USB ID (펌웨어 src/usb_descriptors.c 와 일치)
//   일반 모드 : 0x1209 / 0xc000
//   설정 모드 : 0x2e8a / 0x107c
// 포트 선택 창에 이 장치들만 보이게 해서 "어느 COM 포트인지" 고민을 없앤다.
const DESKHOP_IDS = [
  { usbVendorId: 0x1209, usbProductId: 0xc000 },
  { usbVendorId: 0x2e8a, usbProductId: 0x107c },
];

function isDeskhopPort(p) {
  try {
    const i = p && p.getInfo ? p.getInfo() : {};
    return DESKHOP_IDS.some((f) => f.usbVendorId === i.usbVendorId && f.usbProductId === i.usbProductId);
  } catch (_) { return false; }
}

const state = {
  ports: [],           // 사용 권한이 부여된 SerialPort 들
  port: null,          // 현재 선택된 SerialPort
  tunnel: null,        // 연결된 Tunnel
  connecting: null,    // 연결 시도 중인 Tunnel
  busy: false,         // 전송/연결 중복 방지
  queue: [],           // [{ kind:'file'|'dir', file?, files?, label }]
  pending: [],         // 현재 선택된 전송 대상(파일/폴더) — 대기열과 별개
  dirHandle: null,     // 저장 폴더(File System Access)
  cancel: null,        // AbortController
  sendLast: 0,
  recvLast: 0,
  autoTries: 0,        // 자동 연결 재시도 횟수
};

// ----------------------------- 로그 -----------------------------
function log(msg) {
  const box = el('log');
  const t = new Date().toLocaleTimeString();
  box.value += `[${t}] ${msg}\r\n`;
  box.scrollTop = box.scrollHeight;
}

// --------------------- 저장 폴더 기억(IndexedDB) ---------------------
// File System Access 핸들은 IndexedDB 에만 저장할 수 있다. 저장해 두면 다음에
// 페이지를 열었을 때 권한이 남아 있는 경우 조용히 복원되어, 클릭 없이 자동 연결된다.
const IDB_NAME = 'dhweb';
const IDB_STORE = 'kv';

function idbOpen() {
  return new Promise((res, rej) => {
    if (!('indexedDB' in window)) { rej(new Error('IndexedDB 미지원')); return; }
    const req = indexedDB.open(IDB_NAME, 1);
    req.onupgradeneeded = () => { if (!req.result.objectStoreNames.contains(IDB_STORE)) req.result.createObjectStore(IDB_STORE); };
    req.onsuccess = () => res(req.result);
    req.onerror = () => rej(req.error || new Error('IndexedDB 열기 실패'));
  });
}

async function idbGet(key) {
  const db = await idbOpen();
  return new Promise((res, rej) => {
    const tx = db.transaction(IDB_STORE, 'readonly');
    const q = tx.objectStore(IDB_STORE).get(key);
    q.onsuccess = () => res(q.result);
    q.onerror = () => rej(q.error);
  });
}

async function idbSet(key, value) {
  const db = await idbOpen();
  return new Promise((res, rej) => {
    const tx = db.transaction(IDB_STORE, 'readwrite');
    tx.objectStore(IDB_STORE).put(value, key);
    tx.oncomplete = () => res(true);
    tx.onerror = () => rej(tx.error);
  });
}

// 이전에 고른 저장 폴더를 조용히 되살린다(권한이 아직 남아 있을 때만).
async function restoreSaveDir() {
  try {
    const h = await idbGet('saveDir');
    if (!h || !h.queryPermission) return false;
    const p = await h.queryPermission({ mode: 'readwrite' });
    if (p !== 'granted') return false;          // 재허용은 클릭이 필요하다
    state.dirHandle = h;
    el('saveDirText').value = h.name;
    log('저장 폴더 복원: ' + h.name);
    return true;
  } catch (_) { return false; }
}

// ----------------------------- 포트 -----------------------------
async function refreshPorts() {
  if (!('serial' in navigator)) {
    log('이 브라우저는 Web Serial 을 지원하지 않습니다. Chrome/Edge(또는 Firefox 151+)를 사용하세요.');
    return;
  }
  try {
    const granted = await navigator.serial.getPorts();
    // 기존 목록과 병합(중복 제거)
    for (const p of granted) if (!state.ports.includes(p)) state.ports.push(p);
    renderPorts();
    selectPreferredPort();
    log(`포트 ${state.ports.length}개 사용 가능` +
      (state.ports.length ? '' : ' — 보드를 USB 로 연결하고 [포트 추가…]를 한 번 누르면 이후에는 자동입니다.'));
  } catch (e) {
    log('포트 조회 실패: ' + e.message);
  }
}

// 이미 선택된 포트가 유효하면 그대로, 아니면 DeskHop 포트를 우선 선택한다.
function selectPreferredPort() {
  const sel = el('portSel');
  const cur = sel.selectedIndex;
  if (cur >= 0 && cur < state.ports.length) return state.ports[cur];
  if (!state.ports.length) return null;
  const i = state.ports.findIndex(isDeskhopPort);
  sel.selectedIndex = i >= 0 ? i : state.ports.length - 1;
  return state.ports[sel.selectedIndex];
}

function portLabel(p, i) {
  const info = p.getInfo ? p.getInfo() : {};
  const vid = info.usbVendorId != null ? info.usbVendorId.toString(16).padStart(4, '0') : '----';
  const pid = info.usbProductId != null ? info.usbProductId.toString(16).padStart(4, '0') : '----';
  return `포트 ${i + 1} (VID:${vid} PID:${pid})`;
}

function renderPorts() {
  const sel = el('portSel');
  const prev = sel.selectedIndex;
  sel.innerHTML = '';
  state.ports.forEach((p, i) => {
    const o = document.createElement('option');
    o.value = String(i); o.textContent = portLabel(p, i);
    sel.appendChild(o);
  });
  if (prev >= 0 && prev < state.ports.length) sel.selectedIndex = prev;
  else if (state.ports.length) sel.selectedIndex = state.ports.length - 1;
}

// 필터를 걸면 선택 창에 DeskHop 보드만 뜬다.
// Shift+클릭(또는 필터로 못 찾을 때)은 전체 포트를 보여준다.
async function addPort(allPorts) {
  if (!('serial' in navigator)) { log('Web Serial 미지원 브라우저입니다.'); return; }
  const opts = allPorts ? {} : { filters: DESKHOP_IDS };
  try {
    const p = await navigator.serial.requestPort(opts);
    if (!state.ports.includes(p)) state.ports.push(p);
    renderPorts();
    el('portSel').selectedIndex = state.ports.indexOf(p) >= 0 ? state.ports.indexOf(p) : state.ports.length - 1;
    log('포트 추가됨: ' + portLabel(p, state.ports.indexOf(p)));
    await autoConnectIfPossible('포트 추가됨');
  } catch (e) {
    if (e && e.name === 'NotFoundError') {
      log(allPorts
        ? '포트 선택이 취소되었습니다.'
        : 'DeskHop 보드가 목록에 없습니다 — USB 연결을 확인하거나 Shift+클릭으로 전체 포트를 보세요.');
      return;
    }
    log('포트 추가 실패: ' + (e && e.message ? e.message : e));
  }
}

function selectedPort() {
  const i = el('portSel').selectedIndex;
  return i >= 0 && i < state.ports.length ? state.ports[i] : null;
}

// ----------------------------- 저장 폴더 -----------------------------
async function pickSaveDir() {
  if (!window.showDirectoryPicker) {
    log('이 브라우저는 저장 폴더 선택(File System Access)을 지원하지 않습니다. Windows 11 에서는 Edge/Chrome 을 사용하세요.');
    return;
  }
  try {
    const h = await window.showDirectoryPicker({ mode: 'readwrite' });
    const perm = await h.requestPermission({ mode: 'readwrite' });
    if (perm !== 'granted') { log('저장 폴더 쓰기 권한이 거부되었습니다.'); return; }
    state.dirHandle = h;
    el('saveDirText').value = h.name;
    log('저장 폴더: ' + h.name + ' (폴더/이어받기 사용 가능)');
    try { await idbSet('saveDir', h); } catch (_) { }
    await autoConnectIfPossible('저장 폴더 선택됨');
  } catch (e) {
    if (e && e.name === 'AbortError') return;
    log('저장 폴더 선택 실패: ' + (e && e.message ? e.message : e));
  }
}

// ------------------------- 자동 연결 -------------------------
// 페이지를 열면 부여된 COM 포트를 자동으로 고르고, 저장 폴더까지 준비돼 있으면
// 바로 터널을 시작한다(상대편이 늦어도 connectWait 로 계속 기다린다).
const AUTO_KEY = 'dhweb.autoConnect';
let autoRetryTimer = null;

function autoOn() {
  const c = el('autoConnect');
  return !!(c && c.checked);
}

function loadAutoPref() {
  const c = el('autoConnect');
  if (!c) return;
  try {
    const v = localStorage.getItem(AUTO_KEY);
    c.checked = v === null ? true : v === '1';
  } catch (_) { c.checked = true; }
}

function saveAutoPref() {
  try { localStorage.setItem(AUTO_KEY, autoOn() ? '1' : '0'); } catch (_) { }
}

function scheduleAutoRetry() {
  if (autoRetryTimer || !autoOn()) return;
  const n = ++state.autoTries;
  if (n > 20) {
    log('자동 연결 재시도 중지(20회) — 포트가 다른 앱에 잠겨 있는지 확인하세요.');
    return;
  }
  const delay = Math.min(3000 + n * 1000, 15000);
  autoRetryTimer = setTimeout(() => {
    autoRetryTimer = null;
    autoConnectIfPossible(`재시도 ${n}`);
  }, delay);
}

async function autoConnectIfPossible(reason) {
  if (!autoOn()) return false;
  if (!('serial' in navigator)) return false;
  if (state.tunnel || state.connecting || state.busy) return false;

  const port = selectPreferredPort();
  if (!port) {
    log('자동 연결 대기 — 아직 허용된 COM 포트가 없습니다. [포트 추가…]를 한 번만 누르면 끝입니다.');
    return false;
  }
  if (!state.dirHandle) {
    log('COM 포트는 자동 선택됨 — [저장 폴더 선택]을 하면 자동으로 연결됩니다.');
    return false;
  }

  log('자동 연결 시작' + (reason ? ` (${reason})` : ''));
  el('waitPeer').checked = true;        // 상대편이 늦어도 계속 기다린다
  const ok = await startConnect(true);
  if (ok) {
    state.autoTries = 0;
    if (autoRetryTimer) { clearTimeout(autoRetryTimer); autoRetryTimer = null; }
  } else {
    scheduleAutoRetry();
  }
  return ok;
}

// ----------------------------- 연결 -----------------------------
function setConnected(on) {
  el('connectBtn').textContent = on ? 'CDC 터널 해제' : 'CDC 터널 연결';
}

async function onConnect() {
  if (state.tunnel || state.connecting) { await disconnectTunnel(); return; }
  state.autoTries = 0;
  if (autoRetryTimer) { clearTimeout(autoRetryTimer); autoRetryTimer = null; }
  await startConnect(el('waitPeer').checked);
}

async function onWaitToggle() {
  if (el('waitPeer').checked) { await startConnect(true); return; }
  if (state.tunnel || state.connecting) await disconnectTunnel();
}

async function startConnect(wait) {
  const port = selectedPort();
  if (!port) { log("COM 포트를 선택하지 않았습니다. '포트 추가'로 DeskHop CDC 포트를 선택하세요."); el('waitPeer').checked = false; return false; }
  if (!window.showDirectoryPicker) {
    log('저장 폴더 선택(File System Access)을 지원하지 않는 브라우저입니다. 수신이 불가하므로 Windows 11 에서는 Edge/Chrome 을 사용하세요.');
    el('waitPeer').checked = false; return false;
  }
  if (!state.dirHandle) {
    // 수신은 항상 저장 폴더로만 기록한다. 연결 전에 반드시 선택하게 한다.
    await pickSaveDir();
    if (!state.dirHandle) { log('저장 폴더를 선택해야 연결할 수 있습니다.'); el('waitPeer').checked = false; return false; }
  }
  if (state.busy) { log('이미 연결 중이거나 전송 중입니다.'); if (wait) el('waitPeer').checked = false; return false; }
  state.busy = true; el('connectBtn').disabled = true;

  const recvProg = (p) => updateRecvProgress(p);
  const serial = new Serial(port);

  try {
    await serial.open(6000000);
    await serial.resetDTR();
    log(`CDC 포트 열림 (${portLabel(port, state.ports.indexOf(port))})`);
  } catch (e) {
    log('포트 열기 실패: ' + (e && e.message ? e.message : e) + ' — 다른 앱이 COM 포트를 잡고 있지 않은지 확인하세요.');
    try { await serial.close(); } catch (_) { }
    state.busy = false; el('connectBtn').disabled = false; el('waitPeer').checked = false;
    return false;
  }

  const tunnel = new Tunnel(serial, { dirHandle: state.dirHandle, onLog: log, onRecvProgress: recvProg });
  tunnel.start();
  state.connecting = tunnel;

  log((wait ? '연결 대기 시작' : 'CDC 터널 연결 시도') + ' — 상대편에서도 연결하면 자동으로 이어집니다.');
  try {
    if (wait) await tunnel.connectWait(); else await tunnel.connect();
  } catch (e) {
    log('터널 연결 실패: ' + (e && e.message ? e.message : e));
    await tunnel.close();
    state.connecting = null;
    state.busy = false; el('connectBtn').disabled = false; el('waitPeer').checked = false;
    return false;
  }

  state.connecting = null;
  state.tunnel = tunnel;
  setConnected(true);
  el('waitPeer').checked = true;
  log('CDC 터널 연결됨 (수신 대기 중)');
  const [local, peer] = tunnel.roles();
  if (local === '') log('역할 배너를 받지 못했습니다 — 양쪽 펌웨어가 새 버전인지 확인하세요.');
  updateRoleLabel();
  state.busy = false; el('connectBtn').disabled = false;
  watchRoles(tunnel);
  return true;
}

async function disconnectTunnel() {
  // 수동 해제는 "자동 연결 그만" 의사로 본다(로그에 남기고 체크 해제).
  const cAuto = el('autoConnect');
  if (cAuto && cAuto.checked) {
    cAuto.checked = false; saveAutoPref();
    log('자동 연결을 껐습니다 (다시 켜려면 [자동 연결] 체크).');
  }
  if (autoRetryTimer) { clearTimeout(autoRetryTimer); autoRetryTimer = null; }
  const t = state.tunnel, c = state.connecting;
  state.tunnel = null; state.connecting = null;
  if (t) await t.close();
  else if (c) await c.close();
  setConnected(false);
  el('waitPeer').checked = false;
  el('roleLabel').textContent = '역할: (미연결)';
  state.busy = false; el('connectBtn').disabled = false;
  log('CDC 터널 연결을 해제했습니다.');
}

function updateRoleLabel() {
  const t = state.tunnel; if (!t) return;
  const [local, peer] = t.roles();
  let txt = `역할: 이 창=${sideName(local)} · 상대=${sideName(peer)}`;
  if (local !== '' && local === peer) txt += '  ⚠ 양쪽이 같은 역할(자동 판정 확인 필요)';
  el('roleLabel').textContent = txt;
}

async function watchRoles(t) {
  while (state.tunnel === t) {
    updateRoleLabel();
    if (t.closed) { await onTunnelDropped(t); return; }
    await sleep(500);
  }
}

// 상대편 전원/USB 가 빠지거나 보드가 리셋되어 터널이 스스로 닫힌 경우.
// (사용자가 버튼으로 해제한 경우는 disconnectTunnel 이 먼저 state.tunnel 을 지우므로 여기 오지 않는다.)
async function onTunnelDropped(t) {
  if (state.tunnel !== t) return;
  state.tunnel = null;
  setConnected(false);
  el('waitPeer').checked = false;
  el('roleLabel').textContent = '역할: (미연결)';
  state.busy = false; el('connectBtn').disabled = false;
  log('CDC 터널이 끊어졌습니다.');
  if (autoOn()) {
    log('자동 연결이 켜져 있어 다시 연결합니다…');
    await refreshPorts();
    await autoConnectIfPossible('연결 끊김 후 재연결');
  }
}

// ----------------------------- 파일/폴더 선택 -----------------------------
function pickFiles() {
  el('fileInput').value = '';
  el('fileInput').click();
}
function pickFolder() {
  el('dirInput').value = '';
  el('dirInput').click();
}

// 선택 결과를 "현재 대상"(pending)으로 잡는다. 대기열에는 '대기열에 추가'로 넣는다.
function onFilesPicked(input, kind) {
  const files = Array.from(input.files || []);
  if (!files.length) return;
  if (kind === 'file') {
    state.pending = files.map((f) => ({ kind: 'file', file: f, label: f.name }));
    el('fileText').value = files.map((f) => f.name).join(', ');
  } else {
    const name = (files[0].webkitRelativePath || files[0].name).split('/')[0];
    state.pending = [{ kind: 'dir', files, name, label: name + '/' }];
    el('fileText').value = name + '/';
  }
  log('전송 대상 선택: ' + state.pending.map((p) => p.label).join(', '));
}

// -------------------- 붙여넣기 / 드래그&드롭 (파일 1개일 때만) --------------------
//
//  규칙: 클립보드/드롭에 "파일이 정확히 1개" 있을 때만 이 앱이 가로채서 터널 전송으로 넘긴다.
//        그 외(0개 · 2개 이상 · 폴더 · 문자열 · 이미지)는 preventDefault 하지 않고
//        브라우저 기본 동작(일반 Ctrl+V 붙여넣기)에 그대로 맡긴다.

// 단일 파일이면 File, 아니면 null.
function singleFileOf(dt) {
  if (!dt) return null;
  const files = Array.from(dt.files || []);
  if (files.length !== 1) return null;                 // 0개 또는 2개 이상 → 가로채지 않음

  const items = Array.from(dt.items || []);
  if (items.length === 1) {
    const it = items[0];
    if (it.kind !== 'file') return null;               // 문자열 등 → 기본 동작
    let entry = null;
    try { entry = it.webkitGetAsEntry ? it.webkitGetAsEntry() : null; } catch (_) { }
    if (entry && entry.isDirectory) return null;       // 폴더는 "파일 1개"가 아님
  }

  const f = files[0];
  if (!f || !f.name) return null;
  return f;
}

// 파일 1개를 현재 전송 대상으로 잡고, 연결돼 있으면 바로 전송한다.
async function routeSingleFile(file, source) {
  state.pending = [{ kind: 'file', file, label: file.name }];
  el('fileText').value = file.name;
  log(`${source}: 파일 1개 → 전송으로 넘김 (${file.name}, ${humanBytes(file.size)})`);
  if (!state.tunnel) {
    log("터널이 연결되어 있지 않습니다 — 'CDC 터널 연결' 후 '터널로 전송'을 누르세요.");
    return;
  }
  await onSendFile();
}

function onPaste(e) {
  const f = singleFileOf(e.clipboardData);
  if (!f) return;                                       // 기본 붙여넣기 유지
  e.preventDefault();
  routeSingleFile(f, '붙여넣기');
}

function onDrop(e) {
  const dt = e.dataTransfer;
  const f = singleFileOf(dt);
  e.preventDefault();                                   // 드롭의 기본 동작(파일 열기)은 항상 막는다
  if (!f) {
    const n = dt ? Array.from(dt.files || []).length : 0;
    if (n > 1) log(`드롭한 파일 ${n}개 — 파일이 1개일 때만 전송합니다 (무시됨).`);
    else if (n === 1) log('폴더는 전송 대상이 아닙니다 — 파일 1개를 드롭하세요 (무시됨).');
    return;
  }
  routeSingleFile(f, '드롭');
}

// ----------------------------- 대기열 -----------------------------
function renderQueue() {
  const list = el('queueList');
  list.innerHTML = '';
  state.queue.forEach((it, i) => {
    const row = document.createElement('option');
    row.value = String(i); row.textContent = `${i + 1}) ${it.label}`;
    list.appendChild(row);
  });
  el('queueInfo').textContent = `대기열 ${state.queue.length}개`;
}

function queueAdd() {
  if (!state.pending.length) { log('먼저 보낼 파일/폴더를 선택하세요.'); return; }
  for (const it of state.pending) state.queue.push(it);
  renderQueue();
  log('대기열에 추가: ' + state.pending.map((p) => p.label).join(', '));
}

function queueRemove() {
  const i = el('queueList').selectedIndex;
  if (i < 0 || i >= state.queue.length) return;
  state.queue.splice(i, 1); renderQueue();
}
function queueClear() { state.queue = []; renderQueue(); }

// ----------------------------- 전송 -----------------------------
async function onSendFile() {
  const t = state.tunnel;
  if (!t) { log("먼저 'CDC 터널 연결'을 누르세요."); return; }
  const roots = state.pending.length ? state.pending : (state.queue.length ? [state.queue[state.queue.length - 1]] : []);
  if (!roots.length) { log('보낼 파일/폴더를 선택하세요.'); return; }
  await runSend(t, roots, false);
}

async function onQueueSend() {
  const t = state.tunnel;
  if (!t) { log("먼저 'CDC 터널 연결'을 누르세요."); return; }
  if (!state.queue.length) { log('대기열이 비어 있습니다.'); return; }
  await runSend(t, state.queue.slice(), true);
}

async function runSend(t, roots, isQueue) {
  if (state.busy) { log('이미 연결 중이거나 전송 중입니다.'); return; }
  state.busy = true; el('connectBtn').disabled = true;
  state.cancel = new AbortController();
  const signal = state.cancel.signal;

  let remaining = [];
  let cancelled = false;
  try {
    for (let i = 0; i < roots.length; i++) {
      if (signal.aborted) { remaining = roots.slice(i); cancelled = true; break; }
      const root = roots[i];
      log(`${isQueue ? `대기열 [${i + 1}/${roots.length}] ` : ''}전송 시작: ${root.label}`);
      try {
        await t.sendPath(root, signal, isQueue ? i + 1 : 0, isQueue ? roots.length : 0, (p) => updateSendProgress(p));
        log('완료: ' + root.label);
      } catch (e) {
        if (e.kind === ERR_CANCELLED) { remaining = roots.slice(i); cancelled = true; break; }
        log('실패: ' + root.label + ' — ' + (e && e.message ? e.message : e));
        remaining.push(root);
      }
    }
  } finally {
    state.busy = false; el('connectBtn').disabled = false;
  }

  if (isQueue) { state.queue = remaining; renderQueue(); }
  if (cancelled) log(`전송 취소 — 남은 항목 ${remaining.length}개 (부분 파일은 이어받기용 보존)`);
  else if (isQueue && remaining.length) log(`대기열 종료 — 실패/남은 항목 ${remaining.length}개`);
  else if (isQueue) log('대기열 전송 완료');
  else el('sendBar').value = 100;
}

function onCancel() {
  if (state.cancel && !state.cancel.signal.aborted) {
    state.cancel.abort();
    log('전송 취소를 요청했습니다. (부분 파일은 이어받기용 보존)');
  }
}

// ----------------------------- 진행률 -----------------------------
function updateSendProgress(p) {
  const now = Date.now();
  if (!p.complete && now - state.sendLast < 100) return;
  state.sendLast = now;
  el('sendBar').value = percent(p);
  el('sendInfo').textContent = progressText(p);
}
function updateRecvProgress(p) {
  const now = Date.now();
  if (!p.complete && now - state.recvLast < 100) return;
  state.recvLast = now;
  el('recvBar').value = percent(p);
  el('recvInfo').textContent = progressText(p);
}
function percent(p) {
  if (!p.overallTotal) return p.complete ? 100 : 0;
  return Math.max(0, Math.min(100, Math.round((p.overallDone * 100) / p.overallTotal)));
}
function progressText(p) {
  const prefix = p.queueTotal > 0 ? `대기열[${p.queueIndex}/${p.queueTotal}] ` : '';
  const resumed = p.resumed ? ' (이어받기)' : '';
  if (p.complete) return `${prefix}완료 [${p.index}/${p.total}] ${p.name}${resumed}`;
  return `${prefix}[${p.index}/${p.total}] ${p.name}  ${humanBytes(p.overallDone)} / ${humanBytes(p.overallTotal)}${resumed}`;
}

// ----------------------------- 초기화 -----------------------------
async function init() {
  if (!('serial' in navigator)) {
    log('경고: 이 브라우저는 Web Serial 을 지원하지 않습니다. Windows 11 에서는 Edge 또는 Chrome 을 사용하세요.');
  }
  if (!window.showDirectoryPicker) {
    log('경고: 이 브라우저는 저장 폴더 선택(File System Access)을 지원하지 않습니다. 수신이 불가하므로 Windows 11 에서는 Edge/Chrome 을 사용하세요.');
  }

  // Shift+클릭 = 필터 없이 전체 포트 보기
  el('addPortBtn').addEventListener('click', (e) => addPort(!!(e && e.shiftKey)));
  el('refreshBtn').addEventListener('click', async () => { await refreshPorts(); await autoConnectIfPossible('새로고침'); });
  el('connectBtn').addEventListener('click', onConnect);
  el('waitPeer').addEventListener('change', onWaitToggle);

  loadAutoPref();
  el('autoConnect').addEventListener('change', async () => {
    saveAutoPref();
    if (autoOn()) { state.autoTries = 0; await autoConnectIfPossible('자동 연결 켜짐'); }
    else { if (autoRetryTimer) { clearTimeout(autoRetryTimer); autoRetryTimer = null; } log('자동 연결을 껐습니다.'); }
  });

  el('pickFileBtn').addEventListener('click', pickFiles);
  el('pickFolderBtn').addEventListener('click', pickFolder);
  el('fileInput').addEventListener('change', (e) => onFilesPicked(e.target, 'file'));
  el('dirInput').addEventListener('change', (e) => onFilesPicked(e.target, 'dir'));
  el('sendBtn').addEventListener('click', onSendFile);

  // 클립보드/드롭에 파일이 정확히 1개일 때만 가로채서 전송한다.
  // (문자열·여러 개·폴더는 preventDefault 하지 않으므로 브라우저 기본 동작 그대로)
  document.addEventListener('paste', onPaste);
  const dropArea = document.querySelector('main') || document.body;
  dropArea.addEventListener('dragover', (e) => { e.preventDefault(); });
  dropArea.addEventListener('drop', onDrop);

  el('queueAddBtn').addEventListener('click', queueAdd);
  el('queueRemoveBtn').addEventListener('click', queueRemove);
  el('queueClearBtn').addEventListener('click', queueClear);
  el('queueSendBtn').addEventListener('click', onQueueSend);
  el('cancelBtn').addEventListener('click', onCancel);

  el('pickSaveDirBtn').addEventListener('click', pickSaveDir);

  if ('serial' in navigator) {
    let debounce = null;
    const onChange = (msg) => {
      if (debounce) return;
      debounce = setTimeout(async () => {
        debounce = null;
        log(msg);
        await refreshPorts();
        await autoConnectIfPossible(msg);
      }, 400);
    };
    navigator.serial.addEventListener('connect', () => onChange('시리얼 장치 연결됨'));
    navigator.serial.addEventListener('disconnect', () => onChange('시리얼 장치 분리됨'));
  }

  renderQueue();

  // 저장 폴더 기억 복원 → 포트 조회 → 가능하면 바로 연결
  await restoreSaveDir();
  await refreshPorts();
  await autoConnectIfPossible('페이지 열림');
}

if (document.readyState === 'loading') window.addEventListener('DOMContentLoaded', init);
else init();
