# 3rd/Web/serve.ps1
#
# 설치가 필요 없는 초경량 로컬 HTTP 서버 (Windows PowerShell 내장 기능만 사용).
# 폐쇄망/오프라인 PC에서 Web Serial 을 쓰려면 http://localhost 가 필요하다
# (file:// 는 브라우저가 시리얼 접근을 차단한다).
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\serve.ps1 [-Port 9000] [-NoBrowser] [-SaveDir <경로>]
#
# PC 두 대가 필요로 하는 건 각 PC 의 http://localhost 하나뿐이다(COM 포트와는 무관).
# 배포 파일은 이 스크립트 + transfer.html 2개뿐이다.
#
# 이 스크립트가 있는 폴더를 127.0.0.1:<Port> 에서 서빙하고 브라우저를 연다.
# 관리자 권한 불필요(tcp 직접 바인딩).
#
# 기본 문서는 transfer.html 이다(단일 소스 · 자체완결). / 요청은 자동으로 transfer.html 로 간다.
#
# ■ 수신 파일 저장 (브라우저에서 폴더를 고를 필요 없음)
#   페이지가 받은 바이트를 POST /upload 로 보내면 이 스크립트가 직접 파일로 기록한다.
#   기본 저장 위치 = 이 스크립트가 있는 폴더(= 3rd/Web). -SaveDir 로 바꿀 수 있다.
#
#     GET  /upload/config                              저장 위치와 사용 가능 여부
#     GET  /upload/status?path=<rel>                    이어받기용 .part 크기
#     GET  /upload/part?path=<rel>&max=<n>             .part 앞부분(<n> 바이트) 원본 전송(CRC 검증용)
#     POST /upload?path=<rel>&offset=<n>[&truncate=1]   본문 바이트를 <offset> 위치에 기록
#     POST /upload/finalize?path=<rel>                 <rel>.part → <rel>(중복 시 이름-1.ext)
#     POST /upload/abort?path=<rel>                    <rel>.part 삭제
#     POST /upload/mkdir?path=<rel>                    폴더 생성
#
#   <rel> 은 저장 폴더 기준 상대 경로이며, 저장 폴더 밖으로 나가는 경로는 400 으로 거부한다.
#   로컬(127.0.0.1)에서만 듣기 때문에 외부에 열리지 않는다.

param(
  [int]$Port = 9000,
  [switch]$NoBrowser,
  [string]$SaveDir
)

$ErrorActionPreference = 'Stop'
$port = $Port
$root = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (-not $root) { $root = (Get-Location).Path }
$rootFull = [System.IO.Path]::GetFullPath($root)

# 수신 파일이 기록될 위치. 기본값은 이 스크립트가 있는 폴더.
if ([string]::IsNullOrWhiteSpace($SaveDir)) { $saveRoot = $rootFull }
else {
  $saveRoot = [System.IO.Path]::GetFullPath($SaveDir)
}
$saveRoot = $saveRoot.TrimEnd('\', '/')
if ($saveRoot -match '^[A-Za-z]:$') { $saveRoot += '\' }   # 드라이브 루트(D:\) 보호
if (-not (Test-Path -LiteralPath $saveRoot)) {
  try { [void](New-Item -ItemType Directory -Path $saveRoot -Force) } catch { }
}

$mime = @{
  '.html' = 'text/html; charset=utf-8'
  '.htm'  = 'text/html; charset=utf-8'
  '.js'   = 'text/javascript; charset=utf-8'
  '.css'  = 'text/css; charset=utf-8'
  '.json' = 'application/json; charset=utf-8'
  '.svg'  = 'image/svg+xml'
  '.png'  = 'image/png'
  '.ico'  = 'image/x-icon'
  '.txt'  = 'text/plain; charset=utf-8'
  '.md'   = 'text/plain; charset=utf-8'
}

# ----------------------------- 공통 유틸 -----------------------------

function Send-Response($stream, $status, $contentType, [byte[]]$body) {
  if ($null -eq $body) { $body = New-Object byte[] 0 }
  $header = "HTTP/1.1 $status`r`nContent-Type: $contentType`r`nContent-Length: $($body.Length)`r`nCache-Control: no-store`r`nConnection: close`r`n`r`n"
  $hb = [System.Text.Encoding]::ASCII.GetBytes($header)
  $stream.Write($hb, 0, $hb.Length)
  if ($body.Length -gt 0) { $stream.Write($body, 0, $body.Length) }
  $stream.Flush()
}

# JSON 문자열 값으로 안전하게 감싼다.
function Q([string]$s) {
  if ($null -eq $s) { $s = '' }
  $t = $s.Replace('\', '\\').Replace('"', '\"').Replace("`r", '\r').Replace("`n", '\n').Replace("`t", '\t')
  return '"' + $t + '"'
}

function Send-Json($stream, $status, [string]$json) {
  Send-Response $stream $status 'application/json; charset=utf-8' ([System.Text.Encoding]::UTF8.GetBytes($json))
}

function Send-Err($stream, $status, [string]$msg) {
  Send-Json $stream $status ('{"ok":false,"error":' + (Q $msg) + '}')
}

# 요청 본문(Content-Length) 소비. $target 이 있으면 그 스트림에 기록한다.
function Copy-BodyToStream($stream, [long]$len, $target) {
  if ($len -le 0) { return [long]0 }
  $buf = New-Object byte[] 65536
  $left = $len
  while ($left -gt 0) {
    $want = [int][Math]::Min([int64]$buf.Length, $left)
    $n = $stream.Read($buf, 0, $want)
    if ($n -le 0) { break }
    if ($target) { $target.Write($buf, 0, $n) }
    $left -= $n
  }
  return ($len - $left)
}

# 헤더 블록을 바이트 단위로 읽는다(본문 바이트를 읽어버리지 않도록).
function Read-Head($stream) {
  $bytes = New-Object System.Collections.Generic.List[byte]
  $one = New-Object byte[] 1
  while ($true) {
    $n = $stream.Read($one, 0, 1)
    if ($n -le 0) { break }
    $bytes.Add($one[0])
    # 헤더 끝 판정: CRLFCRLF(…0D 0A 0D 0A) 또는 LF-LF(…0A 0A).
    # 마지막 바이트가 LF 일 때만 검사하므로 본문 첫 바이트를 앞질러 읽지 않는다.
    if ($one[0] -eq 10) {
      $c = $bytes.Count
      if ($c -ge 2 -and $bytes[$c - 2] -eq 10) { break }                                  # \n\n
      if ($c -ge 4 -and $bytes[$c - 2] -eq 13 -and $bytes[$c - 3] -eq 10 -and $bytes[$c - 4] -eq 13) { break }  # \r\n\r\n
    }
    if ($bytes.Count -gt 65536) { break }
  }
  if ($bytes.Count -eq 0) { return $null }
  return [System.Text.Encoding]::ASCII.GetString($bytes.ToArray())
}

function Get-Query([string]$qs) {
  $h = @{}
  if ([string]::IsNullOrEmpty($qs)) { return $h }
  foreach ($kv in $qs.Split('&')) {
    if (-not $kv) { continue }
    $i = $kv.IndexOf('=')
    if ($i -lt 0) { $h[[System.Uri]::UnescapeDataString($kv)] = '' }
    else { $h[[System.Uri]::UnescapeDataString($kv.Substring(0, $i))] = [System.Uri]::UnescapeDataString($kv.Substring($i + 1)) }
  }
  return $h
}

# 저장 폴더 기준 상대 경로 → 절대 경로. 폴더를 벗어나거나 이상한 문자는 $null.
function Resolve-RelPath([string]$rel) {
  if ([string]::IsNullOrWhiteSpace($rel)) { return $null }
  $r = ($rel -replace '\\', '/').Trim('/')
  if (-not $r) { return $null }
  $bad = [char[]]'<>:"|?*'
  foreach ($seg in $r.Split('/')) {
    if (-not $seg) { return $null }
    if ($seg -eq '.' -or $seg -eq '..') { return $null }
    if ($seg.IndexOfAny($bad) -ge 0) { return $null }
  }
  try { $full = [System.IO.Path]::GetFullPath((Join-Path $saveRoot $r)) } catch { return $null }
  $sep = $saveRoot + [System.IO.Path]::DirectorySeparatorChar
  if (-not $full.StartsWith($sep, [System.StringComparison]::OrdinalIgnoreCase)) { return $null }
  return $full
}

# 같은 이름이 있으면 name-1.ext, name-2.ext … 로 비켜 간다(브라우저 쪽 규칙과 동일).
function Get-UniqueName([string]$dir, [string]$name) {
  if (-not (Test-Path -LiteralPath (Join-Path $dir $name))) { return $name }
  $dot = $name.LastIndexOf('.')
  $base = if ($dot -gt 0) { $name.Substring(0, $dot) } else { $name }
  $ext = if ($dot -gt 0) { $name.Substring($dot) } else { '' }
  for ($i = 1; $i -lt 10000; $i++) {
    $cand = "$base-$i$ext"
    if (-not (Test-Path -LiteralPath (Join-Path $dir $cand))) { return $cand }
  }
  return "$base-$([DateTime]::Now.Ticks)$ext"
}

function Get-PartLength([string]$full) {
  $p = $full + '.part'
  if (Test-Path -LiteralPath $p -PathType Leaf) {
    try { return (Get-Item -LiteralPath $p).Length } catch { return [long]0 }
  }
  return [long]0
}

# --------------------------- 업로드 엔드포인트 ---------------------------

function Handle-UploadConfig($stream) {
  Send-Json $stream '200 OK' ('{"ok":true,"root":' + (Q $saveRoot) + ',"port":' + $port + '}')
}

function Handle-UploadStatus($stream, $query) {
  $full = Resolve-RelPath $query['path']
  if (-not $full) { return (Send-Err $stream '400 Bad Request' 'bad path') }
  Send-Json $stream '200 OK' ('{"ok":true,"part":' + (Get-PartLength $full) + '}')
}

# 이어받기 검증용: .part 의 앞부분(<= max 바이트)을 원본 그대로 돌려준다.
function Handle-UploadPart($stream, $query) {
  $full = Resolve-RelPath $query['path']
  if (-not $full) { return (Send-Err $stream '400 Bad Request' 'bad path') }
  $max = [long]0
  if ($query.ContainsKey('max')) { [void][long]::TryParse($query['max'], [ref]$max) }
  $p = $full + '.part'
  if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { return (Send-Err $stream '404 Not Found' 'no part') }
  $len = [long](Get-Item -LiteralPath $p).Length
  if ($max -gt 0 -and $max -lt $len) { $len = $max }
  $fs = New-Object System.IO.FileStream($p, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
  try {
    $header = "HTTP/1.1 200 OK`r`nContent-Type: application/octet-stream`r`nContent-Length: $len`r`nCache-Control: no-store`r`nConnection: close`r`n`r`n"
    $hb = [System.Text.Encoding]::ASCII.GetBytes($header)
    $stream.Write($hb, 0, $hb.Length)
    $buf = New-Object byte[] 65536
    $left = $len
    while ($left -gt 0) {
      $want = [int][Math]::Min([int64]$buf.Length, $left)
      $n = $fs.Read($buf, 0, $want)
      if ($n -le 0) { break }
      $stream.Write($buf, 0, $n)
      $left -= $n
    }
    $stream.Flush()
  }
  finally { try { $fs.Dispose() } catch { } }
}

function Handle-UploadChunk($stream, $query, [long]$contentLength) {
  $full = Resolve-RelPath $query['path']
  if (-not $full) {
    [void](Copy-BodyToStream $stream $contentLength $null)
    return (Send-Err $stream '400 Bad Request' 'bad path')
  }
  $offset = [long]0
  if ($query.ContainsKey('offset')) { [void][long]::TryParse($query['offset'], [ref]$offset) }
  $truncate = ($query.ContainsKey('truncate') -and $query['truncate'] -eq '1')

  $dir = [System.IO.Path]::GetDirectoryName($full)
  if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }

  $p = $full + '.part'
  $fs = $null
  $err = $null
  $size = [long]0
  try {
    $fs = New-Object System.IO.FileStream($p, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::Read)
    $cur = [long]$fs.Length
    if ($truncate) { $fs.SetLength(0); $cur = [long]0 }
    if ($offset -gt $cur) {
      $err = "offset gap ($offset > $cur)"
      [void](Copy-BodyToStream $stream $contentLength $null)
    }
    else {
      if ($offset -lt $cur) { $fs.SetLength($offset) }
      $fs.Position = $offset
      [void](Copy-BodyToStream $stream $contentLength $fs)
      $fs.Flush()
    }
    $size = [long]$fs.Length
  }
  catch {
    $err = $_.Exception.Message
    [void](Copy-BodyToStream $stream $contentLength $null)
  }
  finally { if ($fs) { try { $fs.Dispose() } catch { } } }

  if ($err) { return (Send-Err $stream '409 Conflict' $err) }
  Send-Json $stream '200 OK' ('{"ok":true,"size":' + $size + '}')
}

function Handle-UploadFinalize($stream, $query, [long]$contentLength) {
  [void](Copy-BodyToStream $stream $contentLength $null)
  $full = Resolve-RelPath $query['path']
  if (-not $full) { return (Send-Err $stream '400 Bad Request' 'bad path') }
  $p = $full + '.part'
  if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { return (Send-Err $stream '404 Not Found' 'no part') }
  $dir = [System.IO.Path]::GetDirectoryName($full)
  $name = Get-UniqueName $dir ([System.IO.Path]::GetFileName($full))
  $dest = Join-Path $dir $name
  try { [System.IO.File]::Move($p, $dest) }
  catch { return (Send-Err $stream '500 Internal Server Error' $_.Exception.Message) }
  $size = [long]0
  try { $size = [long](Get-Item -LiteralPath $dest).Length } catch { }
  Send-Json $stream '200 OK' ('{"ok":true,"name":' + (Q $name) + ',"size":' + $size + '}')
}

function Handle-UploadAbort($stream, $query, [long]$contentLength) {
  [void](Copy-BodyToStream $stream $contentLength $null)
  $full = Resolve-RelPath $query['path']
  if (-not $full) { return (Send-Err $stream '400 Bad Request' 'bad path') }
  $p = $full + '.part'
  try { if (Test-Path -LiteralPath $p) { Remove-Item -LiteralPath $p -Force } }
  catch { return (Send-Err $stream '500 Internal Server Error' $_.Exception.Message) }
  Send-Json $stream '200 OK' '{"ok":true}'
}

function Handle-UploadMkdir($stream, $query, [long]$contentLength) {
  [void](Copy-BodyToStream $stream $contentLength $null)
  $full = Resolve-RelPath $query['path']
  if (-not $full) { return (Send-Err $stream '400 Bad Request' 'bad path') }
  try { if (-not (Test-Path -LiteralPath $full)) { [void](New-Item -ItemType Directory -Path $full -Force) } }
  catch { return (Send-Err $stream '500 Internal Server Error' $_.Exception.Message) }
  Send-Json $stream '200 OK' '{"ok":true}'
}

# ------------------------------- 시작 -------------------------------

$url = "http://localhost:$port/"
$listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $port)
try {
  $listener.Start()
}
catch {
  # 이미 다른 창에서 serve 가 돌고 있으면 그것을 그대로 쓴다(중복 실행 대비).
  Write-Host ""
  Write-Host "  포트 $port 를 이미 사용 중입니다 — 기존 서버를 그대로 사용합니다: $url"
  Write-Host ""
  if (-not $NoBrowser) {
    $opened = $false
    foreach ($exe in @('msedge.exe', 'chrome.exe')) {
      try { Start-Process $exe $url -ErrorAction Stop; $opened = $true; break } catch { }
    }
    if (-not $opened) { try { Start-Process $url } catch { } }
  }
  exit 0
}

Write-Host ""
Write-Host "  DeskHop Web 서버"
Write-Host "  폴더 : $rootFull"
Write-Host "  저장 : $saveRoot   (수신 파일이 이 폴더에 바로 저장됩니다)"
Write-Host "  주소 : $url"
Write-Host "  종료 : 이 창에서 Ctrl+C"
Write-Host ""

# Edge/Chrome 우선으로 브라우저 열기(없으면 기본 브라우저). -NoBrowser 면 열지 않는다(작업 스케줄러/숨김 실행용).
if (-not $NoBrowser) {
  $opened = $false
  foreach ($exe in @('msedge.exe', 'chrome.exe')) {
    try { Start-Process $exe $url -ErrorAction Stop; $opened = $true; break } catch { }
  }
  if (-not $opened) { try { Start-Process $url } catch { } }
}

while ($true) {
  $client = $null
  try {
    $client = $listener.AcceptTcpClient()
    try { $client.ReceiveTimeout = 60000 } catch { }
    $stream = $client.GetStream()

    $head = Read-Head $stream
    if ([string]::IsNullOrEmpty($head)) { $client.Close(); continue }

    $lines = $head -split "`n"
    $requestLine = $lines[0].TrimEnd("`r")
    $headers = @{}
    for ($i = 1; $i -lt $lines.Length; $i++) {
      $line = $lines[$i].TrimEnd("`r")
      if (-not $line) { continue }
      $j = $line.IndexOf(':')
      if ($j -gt 0) { $headers[$line.Substring(0, $j).Trim().ToLower()] = $line.Substring($j + 1).Trim() }
    }

    $parts = $requestLine.Split(' ')
    $method = if ($parts.Length -ge 1) { $parts[0].ToUpperInvariant() } else { 'GET' }
    $target = if ($parts.Length -ge 2) { $parts[1] } else { '/' }
    $qi = $target.IndexOf('?')
    $rawPath = if ($qi -ge 0) { $target.Substring(0, $qi) } else { $target }
    $queryString = if ($qi -ge 0) { $target.Substring($qi + 1) } else { '' }
    $path = [System.Uri]::UnescapeDataString($rawPath)

    $contentLength = [long]0
    if ($headers.ContainsKey('content-length')) { [void][long]::TryParse($headers['content-length'], [ref]$contentLength) }

    if ($path.StartsWith('/upload', [System.StringComparison]::OrdinalIgnoreCase)) {
      $query = Get-Query $queryString
      $key = $path.TrimEnd('/').ToLowerInvariant()
      if ($method -ne 'POST' -and $key -ne '/upload/config' -and $key -ne '/upload/status' -and $key -ne '/upload/part') {
        [void](Copy-BodyToStream $stream $contentLength $null)
        Send-Err $stream '405 Method Not Allowed' 'use POST'
      }
      elseif ($key -eq '/upload/config') { Handle-UploadConfig $stream }
      elseif ($key -eq '/upload/status') { Handle-UploadStatus $stream $query }
      elseif ($key -eq '/upload/part') { Handle-UploadPart $stream $query }
      elseif ($key -eq '/upload') { Handle-UploadChunk $stream $query $contentLength }
      elseif ($key -eq '/upload/finalize') { Handle-UploadFinalize $stream $query $contentLength }
      elseif ($key -eq '/upload/abort') { Handle-UploadAbort $stream $query $contentLength }
      elseif ($key -eq '/upload/mkdir') { Handle-UploadMkdir $stream $query $contentLength }
      else {
        [void](Copy-BodyToStream $stream $contentLength $null)
        Send-Err $stream '404 Not Found' 'unknown upload endpoint'
      }
      $client.Close()
      continue
    }

    if ($path -eq '/' -or $path -eq '') {
      # transfer.html 이 유일한 소스/배포본(단일 HTML). index.html 은 과거 레이아웃 호환용 폴백.
      if (Test-Path (Join-Path $rootFull 'transfer.html')) { $path = '/transfer.html' }
      elseif (Test-Path (Join-Path $rootFull 'index.html')) { $path = '/index.html' }
      else { $path = '/transfer.html' }
    }

    $rel = $path.TrimStart('/')
    $full = [System.IO.Path]::GetFullPath((Join-Path $rootFull $rel))

    # 경로 이탈 방지
    if (-not $full.StartsWith($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
      Send-Response $stream '403 Forbidden' 'text/plain; charset=utf-8' ([System.Text.Encoding]::UTF8.GetBytes('403'))
    }
    elseif (Test-Path $full -PathType Leaf) {
      $bytes = [System.IO.File]::ReadAllBytes($full)
      $ext = [System.IO.Path]::GetExtension($full).ToLower()
      $ct = if ($mime.ContainsKey($ext)) { $mime[$ext] } else { 'application/octet-stream' }
      Send-Response $stream '200 OK' $ct $bytes
    }
    else {
      Send-Response $stream '404 Not Found' 'text/plain; charset=utf-8' ([System.Text.Encoding]::UTF8.GetBytes('404 Not Found'))
    }
    $client.Close()
  }
  catch {
    if ($client) { try { $client.Close() } catch { } }
  }
}
