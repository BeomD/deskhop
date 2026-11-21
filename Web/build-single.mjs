// Web/build-single.mjs
//
// index.html + style.css + app.js + ui.js 를 하나의 자체완결 HTML 로 합친다.
// 오프라인/폐쇄망 배포용(파일 1개). 생성물: transfer.html
//
//   node Web/build-single.mjs

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = path.dirname(fileURLToPath(import.meta.url));
const read = (f) => fs.readFileSync(path.join(dir, f), 'utf8');

const html = read('index.html');
const css = read('style.css');
const appJs = read('app.js');
const uiJs = read('ui.js');

let out = html
  .replace('<link rel="stylesheet" href="style.css" />', `<style>\n${css}\n</style>`)
  .replace('<script src="app.js"></script>', `<script>\n${appJs}\n</script>`)
  .replace('<script src="ui.js"></script>', `<script>\n${uiJs}\n</script>`)
  .replace('<title>Transfer (Web) — DeskHop CDC 파일 전송</title>',
    '<title>Transfer (Web, standalone) — DeskHop CDC 파일 전송</title>');

if (out.includes('href="style.css"') || out.includes('src="app.js"') || out.includes('src="ui.js"')) {
  throw new Error('치환 실패: index.html 의 참조 구문을 확인하세요');
}

const banner = '<!-- 자동 생성 파일(build-single.mjs). 직접 수정하지 말고 Web/ 소스를 고치세요. -->\n';
fs.writeFileSync(path.join(dir, 'transfer.html'), banner + out);
console.log('generated: Web/transfer.html (' + (banner + out).length + ' bytes)');
