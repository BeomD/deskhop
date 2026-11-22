// UI 스모크 테스트 — 실제 브라우저/보드 없이 DOM 만 가짜로 만들어
// init() 이 끝까지 실행되는지(=오타·미정의 함수·잘못된 id 참조가 없는지) 확인한다.
// 단일 소스(3rd/Web/transfer.html)의 ①+② 스크립트 블록을 그대로 실행한다.
// 사용법: node test/uismoke.mjs
import vm from 'node:vm';
import { html, ids, APP, UI, externalRefs } from './load-source.mjs';

function mkEl(id) {
  return {
    id, value: '', textContent: '', innerHTML: '', checked: false, disabled: false,
    selectedIndex: -1, scrollTop: 0, scrollHeight: 0, style: {}, children: [], options: [],
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    addEventListener(t, f) { (this._h || (this._h = {}))[t] = f; },
    dispatch(t, ev) { if (this._h && this._h[t]) return this._h[t](ev || {}); },
    appendChild(c) { this.children.push(c); this.options.push(c); return c; },
    removeChild() {}, remove() {}, setAttribute() {}, focus() {},
    click() { this.dispatch('click'); },
    querySelector: () => null, insertAdjacentHTML() {},
  };
}

const els = new Map(ids.map((i) => [i, mkEl(i)]));
const getEl = (id) => {
  if (!els.has(id)) els.set(id, mkEl(id));
  return els.get(id);
};

const document = {
  readyState: 'complete',
  getElementById: getEl,
  createElement: (tag) => mkEl('<' + tag + '>'),
  addEventListener() {},
  body: mkEl('body'),
  querySelector: () => null,
};

const store = new Map();
const localStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: (k) => store.delete(k),
};

const windowListeners = {};
const window = {
  isSecureContext: true,
  addEventListener: (t, f) => { (windowListeners[t] || (windowListeners[t] = [])).push(f); },
};

// Web Serial 은 있으나 권한이 남은 포트가 0개인 상태(=첫 실행과 동일)
const serial = {
  getPorts: async () => [],
  addEventListener() {},
  requestPort: async () => { throw new Error('stub: no port'); },
};
const navigator = { serial };

const sandbox = {
  console, document, window, navigator, localStorage,
  setTimeout, clearTimeout, setInterval, clearInterval, queueMicrotask,
  TextEncoder, TextDecoder, AbortController, Blob, crypto, performance,
};
sandbox.globalThis = sandbox;
sandbox.self = sandbox;
vm.createContext(sandbox);

let initError = null;
try {
  vm.runInContext(APP, sandbox, { filename: 'transfer.html#core' });
  vm.runInContext(UI, sandbox, { filename: 'transfer.html#ui' });
  // init() 은 async — 타이머/마이크로태스크를 잠깐 돌린다.
  await new Promise((r) => setTimeout(r, 500));
} catch (e) {
  initError = e;
}

const logs = getEl('log').value.split('\r\n').filter(Boolean);
const captures = [];
let unhandled = null;
process.on('unhandledRejection', (e) => { unhandled = e; });

const checks = [];
const ok = (name, cond, extra) => checks.push({ name, pass: !!cond, extra: extra || '' });

ok('init() 이 예외 없이 실행됨', !initError, initError ? String(initError && initError.message) : '');
ok('transfer.html 에 autoConnect 체크박스 존재', ids.includes('autoConnect'));
ok('자동 연결 체크박스 기본 켜짐', getEl('autoConnect').checked === true, 'checked=' + getEl('autoConnect').checked);
ok('시작 로그가 남음', logs.length > 0, logs.length + '줄');
ok('로그에 포트/자동 연결 안내 포함', logs.some((l) => l.includes('포트') || l.includes('자동')), logs.slice(-3).join(' | '));
ok('autoConnect 기본값을 저장소에 덮어쓰지 않음', !store.has('dhweb.autoConnect'), String(store.get('dhweb.autoConnect')));
ok('저장 폴더 핸들 없이도 자동 연결이 예외를 던지지 않음', !initError);
ok('외부 참조(css/js) 없는 자체완결 단일 파일', externalRefs.length === 0, externalRefs.join(', '));
ok('data-part 로 ①/② 스크립트 블록을 모두 찾음', APP.length > 1000 && UI.length > 1000, `${APP.length}/${UI.length}자`);
ok('마크업에 인라인 <style> 포함', html.includes('<style>'));

let failed = 0;
for (const c of checks) {
  if (!c.pass) failed++;
  console.log((c.pass ? 'PASS  ' : 'FAIL  ') + c.name + (c.extra ? '   [' + c.extra + ']' : ''));
}
console.log('\n--- 로그 ---');
for (const l of logs) console.log('  ' + l);
if (unhandled) console.log('\nunhandledRejection: ' + unhandled);
console.log(failed ? `\n${failed} FAILED` : '\nUI SMOKE OK');
process.exit(failed ? 1 : 0);
