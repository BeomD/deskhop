# 3rd/Web/install-autostart.ps1
#
# serve.ps1 을 Windows 로그인 시 자동으로(창 없이) 시작하게 등록/해제한다.
# Web Serial 은 http://localhost 같은 보안 컨텍스트에서만 동작하므로, 두 PC 모두
# "켜져 있는 로컬 서버"가 필요하다. 이걸 등록해 두면 매번 serve.cmd 를 누르지 않아도 된다.
#
#   install-autostart.cmd            → 등록
#   install-autostart.cmd remove     → 해제
#
# 관리자 권한 불필요(사용자 Startup 폴더에 바로가기만 만든다).

param([switch]$Remove)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (-not $root) { $root = (Get-Location).Path }
$root = [System.IO.Path]::GetFullPath($root)

$startup = [Environment]::GetFolderPath('Startup')
if (-not (Test-Path $startup)) { New-Item -ItemType Directory -Path $startup -Force | Out-Null }
$lnk = Join-Path $startup 'DeskHop Web Server.lnk'

if ($Remove) {
  if (Test-Path $lnk) {
    Remove-Item $lnk -Force
    Write-Host "  자동 시작 해제됨: $lnk"
  }
  else {
    Write-Host "  자동 시작이 등록되어 있지 않습니다."
  }
  exit 0
}

$serve = Join-Path $root 'serve.ps1'
if (-not (Test-Path $serve)) { throw "serve.ps1 을 찾을 수 없습니다: $serve" }

$ps = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path $ps)) { $ps = 'powershell.exe' }

$ws = New-Object -ComObject WScript.Shell
$s = $ws.CreateShortcut($lnk)
$s.TargetPath = $ps
$s.Arguments = '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $serve + '" -NoBrowser'
$s.WorkingDirectory = $root
$s.WindowStyle = 7           # 최소화(숨김에 가깝게)
$s.Description = 'DeskHop Web 전송 페이지를 http://localhost:9000 에 서빙'
$s.Save()

Write-Host ""
Write-Host "  등록 완료: $lnk"
Write-Host "  다음 로그인부터 http://localhost:9000/ 이 자동으로 열립니다(창 없음)."
Write-Host "  지금 바로 적용하려면 이 폴더의 serve.cmd 를 한 번 실행하세요."
Write-Host "  해제: install-autostart.cmd remove"
Write-Host ""
