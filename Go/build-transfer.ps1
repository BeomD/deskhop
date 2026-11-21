#!/usr/bin/env pwsh
#
# Build the stripped-down "Transfer" host tool (CDC file transfer only).
# Uses the -tags transfer entry point (main_transfer.go) so it shares all
# tunnel/serial code with KeyTyper without duplication.
#
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -tags transfer -ldflags "-H=windowsgui" -o Transfer.exe .
Write-Host "built: $PSScriptRoot\Transfer.exe"
