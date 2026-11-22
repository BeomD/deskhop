@echo off
rem DeskHop Web - serve.ps1 로그인 시 자동 시작 등록/해제.
rem   install-autostart.cmd           -> 등록
rem   install-autostart.cmd remove    -> 해제
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-autostart.ps1" %*
echo.
pause
