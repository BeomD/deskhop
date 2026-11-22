#!/usr/bin/env pwsh
#
# Build the "AutoDrop" host tool: a tray-less background agent that watches the
# clipboard and forwards a *single* file to the board's USB flash drive.
#
#   · clipboard holds exactly one file  → the file is sent to the board
#   · anything else (text, 0 files, 2+ files, a folder) → plain Ctrl+V
#
# Trigger defaults to Ctrl+Alt+V (so it never fights Explorer's own Ctrl+V).
# Settings live in autodrop.json next to the exe; log in autodrop.log.
#
# Reuses the serial layer (openCDC/listComPorts in KeyTyper.go) and the DW
# protocol client in internal/writer, exactly like Writer.exe.
#
# Firmware must be built with -DDH_USB_DISK=ON (default in this tree).
#
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -tags autodrop -ldflags "-H=windowsgui" -o AutoDrop.exe .
Write-Host "built: $PSScriptRoot\AutoDrop.exe"
