#!/usr/bin/env node
// serve.ps1 + transfer.html → combine.ps1 (배포용 단일 파일)
//
//   node 3rd/Web/build-combined.mjs            # combine.ps1 을 다시 만든다
//   node 3rd/Web/build-combined.mjs --check    # 최신인지 확인만(다르면 exit 1)
//
// serve.ps1 의 BODY:BEGIN ~ BODY:END 구간(유틸 + 업로드 핸들러 + Serve-Entry)을 그대로 복사하고,
// transfer.html 은 base64 로 넣는다. 그래서 두 스크립트의 서버 코드는 언제나 같다.
// 서버 코드를 고칠 때는 serve.ps1 만 고치고 이 스크립트를 다시 돌린다.
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const SERVE = join(here, 'serve.ps1');
const PAGE = join(here, 'transfer.html');
const OUT = join(here, 'combine.ps1');

const BOM = '\ufeff';
const BEGIN = '# === BODY:BEGIN';
const END = '# === BODY:END ===';

const stripBom = (s) => (s.charCodeAt(0) === 0xfeff ? s.slice(1) : s);
const serve = stripBom(readFileSync(SERVE, 'utf8'));
const page = readFileSync(PAGE);

// ---- serve.ps1 에서 param 블록과 부품 구간을 뽑는다 ----
const bodyBegin = serve.indexOf(BEGIN);
const bodyEnd = serve.indexOf(END);
if (bodyBegin < 0 || bodyEnd < 0) throw new Error(`${SERVE}: BODY:BEGIN / BODY:END 마커가 없습니다`);
if (serve.indexOf(BEGIN, bodyBegin + 1) >= 0) throw new Error(`${SERVE}: BODY:BEGIN 이 여러 개입니다`);

const body = serve.slice(serve.indexOf('\n', bodyBegin) + 1, bodyEnd).replace(/\s*$/, '\n');

const paramStart = serve.indexOf('param(');
if (paramStart < 0 || paramStart > bodyBegin) throw new Error(`${SERVE}: param 블록을 찾지 못했습니다`);
const paramTail = serve.slice(paramStart);
const paramEnd = paramTail.indexOf('\n)\n');
if (paramEnd < 0) throw new Error(`${SERVE}: param 블록이 닫히지 않았습니다`);
const paramBlock = paramTail.slice(0, paramEnd + 3);

// ---- transfer.html 을 base64 로 ----
const b64 = page.toString('base64');
const hashes = [];
for (let i = 0; i < b64.length; i += 76) hashes.push(b64.slice(i, i + 76));

const header = `# combine.ps1 — 배포용 단일 파일 (serve.ps1 + transfer.html 내장)
#
#   생성: node 3rd/Web/build-combined.mjs      (직접 고치지 말 것 — serve.ps1 / transfer.html 이 원본)
#
# 페이지를 포함해 파일이 1개뿐이다. 실행하면 내장된 페이지를 임시 폴더에 풀어서 서빙한다.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\\combine.ps1 [-Port 9000] [-NoBrowser] [-SaveDir <경로>]
#
# ■ 저장 위치
#   수신 파일은 -SaveDir(기본: 이 스크립트가 있는 폴더)에 바로 저장된다. 브라우저에서 폴더를 고를 필요가 없다.
#   페이지 자체는 %TEMP%\\deskhop-web-<PID>\\transfer.html 로 풀리지만, 수신 파일은 그쪽에 쓰이지 않는다.
#
# ■ 왜 페이지를 풀어서 서빙하나
#   Web Serial 은 http(s)://localhost 같은 보안 컨텍스트에서만 동작하고, file:// 로는 쓸 수 없다.
#   그래서 "HTML 한 파일"만으로는 안 되고 포트를 여는 프로세스가 반드시 필요하다.
#   대신 페이지를 이 스크립트 안에 넣어 배포 파일을 1개로 줄인 것이 이 파일이다.
#
# ■ 내장 페이지
#   ${page.length} bytes UTF-8 (transfer.html), 아래 base64 는 그 파일과 바이트 단위로 같다.
#
# ■ 업로드 API / 동작은 serve.ps1 과 동일하다 (같은 BODY 구간을 그대로 쓴다).

`;

const payload = `# ------------------------- 내장 페이지 (transfer.html) -------------------------
$htmlB64 = @(
${hashes.map((h) => `  '${h}'`).join('\n')}
) -join ''
$htmlBytes = [System.Convert]::FromBase64String($htmlB64)

# 페이지를 임시 폴더에 풀어 그 폴더를 문서 루트로 쓴다(실행마다 새로 쓴다).
function Get-PageRoot {
  param([byte[]]$Bytes)
  $dir = Join-Path ([System.IO.Path]::GetTempPath()) ('deskhop-web-' + $PID)
  [void](New-Item -ItemType Directory -Path $dir -Force)
  [System.IO.File]::WriteAllBytes((Join-Path $dir 'transfer.html'), $Bytes)
  return $dir
}

`;

const entry = `
# ------------------------------- 진입점 -------------------------------
$scriptRoot = $PSScriptRoot
if (-not $scriptRoot) { $scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Definition }
if (-not $scriptRoot) { $scriptRoot = (Get-Location).Path }
if ([string]::IsNullOrWhiteSpace($SaveDir)) { $SaveDir = $scriptRoot }

$docRoot = Get-PageRoot $htmlBytes
try {
  Serve-Entry -DocRoot $docRoot -SaveDir $SaveDir -Port $Port -NoBrowser:$NoBrowser
}
finally {
  # Ctrl+C 로 끝나도 임시 폴더를 남기지 않는다(실패해도 무시).
  try { Remove-Item -LiteralPath $docRoot -Recurse -Force -ErrorAction Stop } catch { }
  try { [System.IO.Directory]::Delete($docRoot, $true) } catch { }
}
`;

const COPY_BEGIN = '# === BODY:BEGIN  (serve.ps1 의 같은 구간을 그대로 복사한 것 — 여기를 직접 고치지 말 것) ===\n';

const out = BOM + header + paramBlock + '\n' + payload
  + COPY_BEGIN + body + '# === BODY:END ===\n' + entry;

// 생성물 자체 점검: base64 가 원본과 같은가
const back = Buffer.from(hashes.join(''), 'base64');
if (!back.equals(page)) throw new Error('내장 base64 가 transfer.html 과 다릅니다(인코딩 확인)');

if (process.argv.includes('--check')) {
  let cur = '';
  try { cur = readFileSync(OUT, 'utf8'); } catch { }
  if (cur !== out) {
    console.error('combine.ps1 이 최신이 아닙니다 — node 3rd/Web/build-combined.mjs 로 다시 만드세요');
    process.exit(1);
  }
  console.log(`combine.ps1 OK (${page.length} bytes page, ${hashes.length} base64 lines)`);
  process.exit(0);
}

writeFileSync(OUT, out, 'utf8');
console.log(`wrote ${OUT}`);
console.log(`  page : ${page.length} bytes (base64 ${hashes.length} lines)`);
console.log(`  body : ${body.split('\n').length} lines from serve.ps1`);
console.log(`  file : ${Buffer.byteLength(out, 'utf8')} bytes`);
