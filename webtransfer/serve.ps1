# webtransfer/serve.ps1
#
# 설치가 필요 없는 초경량 로컬 HTTP 서버 (Windows PowerShell 내장 기능만 사용).
# 폐쇄망/오프라인 PC에서 Web Serial 을 쓰려면 http://localhost 가 필요하다
# (file:// 는 브라우저가 시리얼 접근을 차단한다).
#
#   serve.cmd 더블클릭  또는
#   powershell -NoProfile -ExecutionPolicy Bypass -File serve.ps1 [-Port 9000]
#
# 이 스크립트가 있는 폴더를 127.0.0.1:<Port> 에서 서빙하고 브라우저를 연다.
# 관리자 권한 불필요(tcp 직접 바인딩).

param([int]$Port = 9000)

$ErrorActionPreference = 'Stop'
$port = $Port
$root = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (-not $root) { $root = (Get-Location).Path }
$rootFull = [System.IO.Path]::GetFullPath($root)

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

$listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $port)
$listener.Start()
$url = "http://localhost:$port/"

Write-Host ""
Write-Host "  DeskHop webtransfer 서버"
Write-Host "  폴더 : $rootFull"
Write-Host "  주소 : $url"
Write-Host "  종료 : 이 창에서 Ctrl+C"
Write-Host ""

# Edge/Chrome 우선으로 브라우저 열기(없으면 기본 브라우저).
$opened = $false
foreach ($exe in @('msedge.exe', 'chrome.exe')) {
  try { Start-Process $exe $url -ErrorAction Stop; $opened = $true; break } catch { }
}
if (-not $opened) { try { Start-Process $url } catch { } }

function Send-Response($stream, $status, $contentType, [byte[]]$body) {
  $header = "HTTP/1.1 $status`r`nContent-Type: $contentType`r`nContent-Length: $($body.Length)`r`nCache-Control: no-store`r`nConnection: close`r`n`r`n"
  $hb = [System.Text.Encoding]::ASCII.GetBytes($header)
  $stream.Write($hb, 0, $hb.Length)
  if ($body.Length -gt 0) { $stream.Write($body, 0, $body.Length) }
  $stream.Flush()
}

while ($true) {
  $client = $null
  try {
    $client = $listener.AcceptTcpClient()
    $stream = $client.GetStream()
    $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::ASCII)
    $requestLine = $reader.ReadLine()
    if ([string]::IsNullOrEmpty($requestLine)) { $client.Close(); continue }
    # 헤더 소비
    while ($true) { $line = $reader.ReadLine(); if ([string]::IsNullOrEmpty($line)) { break } }

    $parts = $requestLine.Split(' ')
    $rawPath = if ($parts.Length -ge 2) { $parts[1] } else { '/' }
    $path = [System.Uri]::UnescapeDataString($rawPath.Split('?')[0])
    if ($path -eq '/' -or $path -eq '') {
      if (Test-Path (Join-Path $rootFull 'index.html')) { $path = '/index.html' }
      elseif (Test-Path (Join-Path $rootFull 'transfer.html')) { $path = '/transfer.html' }
      else { $path = '/index.html' }
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
