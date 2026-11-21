@echo off
rem DeskHop Web — 설치 불필요한 로컬 서버 실행 + 브라우저 열기.
rem 이 파일을 더블클릭하면 serve.ps1 을 실행정책 우회로 실행합니다.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0serve.ps1"
if errorlevel 1 pause
