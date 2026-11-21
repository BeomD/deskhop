#!/usr/bin/env pwsh
#
# Build the "Writer" host tool: sends files straight from this PC to a USB
# flash drive attached to the DeskHop board (no second PC needed).
#
# Uses the -tags writer entry point (main_writer.go / writer_ui.go) so it
# shares the serial layer (openCDC/listComPorts in KeyTyper.go) with the other
# builds; only the entry point and the UI differ.
#
# Firmware must be built with -DDH_USB_DISK=ON (default in this tree).
#
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -tags writer -ldflags "-H=windowsgui" -o Writer.exe .
Write-Host "built: $PSScriptRoot\Writer.exe"