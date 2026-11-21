'use strict';

/* =============================================================================
 *  webtransfer UI — Transfer(GUI) 의 DOM 컨트롤러
 *
 *  UI 하위(lib)는 app.js 의 Serial/Tunnel/progress 유틸을 사용하고, 이 파일은
 *  포트 선택, 대기열, 진행률, 저장 폴더, 취소 등 화면 동작만 담당한다.
 * ============================================================================= */

const el = (id) => document.getElementById(id);

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
};

// ----------------------------- 로그 -----------------------------
function log(msg) {
  const box = el('log');
  const t = new Date().toLocaleTimeString();
  box.value += `[${t}] ${msg}\r\n`;
  box.scrollTop = box.scrollHeight;
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
    log(`포트 ${state.ports.length}개 사용 가능`);
  } catch (e) {
    log('포트 조회 실패: ' + e.message);
  }
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

async function addPort() {
  if (!('serial' in navigator)) { log('Web Serial 미지원 브라우저입니다.'); return; }
  try {
    const p = await navigator.serial.requestPort();
    if (!state.ports.includes(p)) state.ports.push(p);
    renderPorts();
    el('portSel').selectedIndex = state.ports.length - 1;
    log('포트 추가됨. (DeskHop CDC 포트를 선택하세요)');
  } catch (e) {
    if (e && e.name === 'NotFoundError') return; // 사용자가 취소
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
  } catch (e) {
    if (e && e.name === 'AbortError') return;
    log('저장 폴더 선택 실패: ' + (e && e.message ? e.message : e));
  }
}

// ----------------------------- 연결 -----------------------------
function setConnected(on) {
  el('connectBtn').textContent = on ? 'CDC 터널 해제' : 'CDC 터널 연결';
}

async function onConnect() {
  if (state.tunnel || state.connecting) { await disconnectTunnel(); return; }
  await startConnect(false);
}

async function onWaitToggle() {
  if (el('waitPeer').checked) { await startConnect(true); return; }
  if (state.tunnel || state.connecting) await disconnectTunnel();
}

async function startConnect(wait) {
  const port = selectedPort();
  if (!port) { log("COM 포트를 선택하지 않았습니다. '포트 추가'로 DeskHop CDC 포트를 선택하세요."); el('waitPeer').checked = false; return; }
  if (!window.showDirectoryPicker) {
    log('저장 폴더 선택(File System Access)을 지원하지 않는 브라우저입니다. 수신이 불가하므로 Windows 11 에서는 Edge/Chrome 을 사용하세요.');
    el('waitPeer').checked = false; return;
  }
  if (!state.dirHandle) {
    // 수신은 항상 저장 폴더로만 기록한다. 연결 전에 반드시 선택하게 한다.
    await pickSaveDir();
    if (!state.dirHandle) { log('저장 폴더를 선택해야 연결할 수 있습니다.'); el('waitPeer').checked = false; return; }
  }
  if (state.busy) { log('이미 연결 중이거나 전송 중입니다.'); if (wait) el('waitPeer').checked = false; return; }
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
    return;
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
    return;
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
}

async function disconnectTunnel() {
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
    await sleep(500);
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
function init() {
  if (!('serial' in navigator)) {
    log('경고: 이 브라우저는 Web Serial 을 지원하지 않습니다. Windows 11 에서는 Edge 또는 Chrome 을 사용하세요.');
  }
  if (!window.showDirectoryPicker) {
    log('경고: 이 브라우저는 저장 폴더 선택(File System Access)을 지원하지 않습니다. 수신이 불가하므로 Windows 11 에서는 Edge/Chrome 을 사용하세요.');
  }

  el('addPortBtn').addEventListener('click', addPort);
  el('refreshBtn').addEventListener('click', refreshPorts);
  el('connectBtn').addEventListener('click', onConnect);
  el('waitPeer').addEventListener('change', onWaitToggle);

  el('pickFileBtn').addEventListener('click', pickFiles);
  el('pickFolderBtn').addEventListener('click', pickFolder);
  el('fileInput').addEventListener('change', (e) => onFilesPicked(e.target, 'file'));
  el('dirInput').addEventListener('change', (e) => onFilesPicked(e.target, 'dir'));
  el('sendBtn').addEventListener('click', onSendFile);

  el('queueAddBtn').addEventListener('click', queueAdd);
  el('queueRemoveBtn').addEventListener('click', queueRemove);
  el('queueClearBtn').addEventListener('click', queueClear);
  el('queueSendBtn').addEventListener('click', onQueueSend);
  el('cancelBtn').addEventListener('click', onCancel);

  el('pickSaveDirBtn').addEventListener('click', pickSaveDir);

  if ('serial' in navigator) {
    navigator.serial.addEventListener('connect', () => { log('시리얼 장치 연결됨'); refreshPorts(); });
    navigator.serial.addEventListener('disconnect', () => { log('시리얼 장치 분리됨'); refreshPorts(); });
  }

  renderQueue();
  refreshPorts();
}

window.addEventListener('DOMContentLoaded', init);
