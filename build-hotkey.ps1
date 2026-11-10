#!/usr/bin/env pwsh
#
# Build the background Hotkey Sender (no GUI/console, lives in the tray-less
# background; Ctrl+Alt+S sends the clipboard file/screenshot, Ctrl+Alt+Q quits).
#
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -tags hotkey -ldflags "-H=windowsgui" -o HotkeySender.exe .
Write-Host "built: $PSScriptRoot\HotkeySender.exe"
