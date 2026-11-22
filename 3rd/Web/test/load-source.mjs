// 3rd/Web/test/load-source.mjs
//
// 3rd/Web/transfer.html 은 "소스이자 배포본"인 단일 파일이다(빌드 단계 없음).
// 테스트는 파일을 직접 읽지 않고 이 모듈을 통해서만 읽는다 — 마크업/스타일/스크립트
// 블록을 여기서 뽑아 주므로, 나중에 구조가 바뀌어도 수정 지점이 한 곳으로 유지된다.
//
//   import { html, styles, APP, UI, externalRefs } from './load-source.mjs';

import fs from 'node:fs';
import path from 'node:path';

export const ROOT = path.resolve(import.meta.dirname, '..');
export const SOURCE = path.join(ROOT, 'transfer.html');

/** transfer.html 전체 텍스트(id·마크업 검사용) */
export const html = fs.readFileSync(SOURCE, 'utf8');

/** 인라인 <style> 블록들(보통 1개) */
export const styles = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)].map((m) => m[1]);

/** 인라인 <script> 블록들 — 문서 순서 = 실행 순서 */
export const scripts = [...html.matchAll(/<script(?![^>]*\bsrc\s*=)[^>]*>([\s\S]*?)<\/script>/gi)].map((m) => m[1]);

if (scripts.length < 2) {
  throw new Error(`transfer.html 에 인라인 <script> 가 2개 있어야 합니다(현재 ${scripts.length}개)`);
}
if (styles.length === 0) {
  throw new Error('transfer.html 에 인라인 <style> 이 없습니다(단일 소스 위반)');
}

const part = (name) => {
  const m = html.match(new RegExp(`<script[^>]*data-part="${name}"[^>]*>([\\s\\S]*?)<\\/script>`, 'i'));
  if (!m) throw new Error(`transfer.html 에 data-part="${name}" 스크립트 블록이 없습니다`);
  return m[1];
};

/** ① 프로토콜 · 시리얼 · 파일 전송 (Go tunnel.go 포팅) */
export const APP = part('core');
/** ② DOM 컨트롤러 (Go transfer_ui.go 대응) */
export const UI = part('ui');

/** <script src=...> / <link href=...> 같은 외부 참조 목록 — 비어 있어야 자체완결 */
export const externalRefs = [
  ...html.matchAll(/<script[^>]*\bsrc\s*=\s*["']([^"']+)["']/gi),
  ...html.matchAll(/<link[^>]*\bhref\s*=\s*["']([^"']+)["']/gi),
].map((m) => m[1]);

/** 문서 안의 모든 id (가짜 DOM 구성용) */
export const ids = [...html.matchAll(/id="([^"]+)"/g)].map((m) => m[1]);