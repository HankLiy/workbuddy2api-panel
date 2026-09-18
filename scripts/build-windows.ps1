# build-windows.ps1 — 一次构建网关 + 托盘启动器到 dist/
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

New-Item -ItemType Directory -Force dist | Out-Null

Write-Host "==> build wb2api.exe"
go build -trimpath -ldflags="-s -w" -o dist/wb2api.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "==> build wb2api-tray.exe"
go build -trimpath -ldflags="-s -w -H windowsgui" -o dist/wb2api-tray.exe ./cmd/tray
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "==> done: dist/wb2api.exe, dist/wb2api-tray.exe"
Write-Host "部署：两个 exe 放同一目录，连同 config.json / auths/ / data/，双击 wb2api-tray.exe"
