#!/usr/bin/env pwsh
#
# Build the Windows host tool (KeyTyper GUI + CDC file tunnel CLI).
#
# The "-H=windowsgui" linker flag is REQUIRED: without it the GUI runs with an
# extra console window. With it, double-clicking shows only the window, while
# running "KeyTyper.exe tunnel send|recv ..." from a terminal still works (the
# app attaches to the parent console for the CLI).
#
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -ldflags "-H=windowsgui" -o KeyTyper.exe .
Write-Host "built: $PSScriptRoot\KeyTyper.exe"
