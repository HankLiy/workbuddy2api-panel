# build-windows.ps1 — 一次构建网关 + 托盘启动器到 dist/
#
# 关于 -ldflags：只用 -w（去 DWARF 调试信息），**不要加 -s**。
# -s 会删掉 Go 符号表；大体积 Go 二进制一旦无符号表，部分杀软（火绒/卡巴等）
# 的启发式会误判为 HEUR:VirTool/Obfuscator 并删除产物（实测：server -s 被删、-w 存活）。
# -w 仍能显著瘦身（14.4MB → 10.9MB）且不触发误报。若你愿意自行加杀软白名单，
# 再用 -s -w 换那 ~0.7MB。
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

New-Item -ItemType Directory -Force dist | Out-Null

Write-Host "==> build wb2api.exe"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-w" -o dist/wb2api.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "==> build wb2api-tray.exe"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-w -H windowsgui" -o dist/wb2api-tray.exe ./cmd/tray
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "==> done: dist/wb2api.exe, dist/wb2api-tray.exe"
Write-Host "部署：两个 exe 放同一目录，连同 config.json / auths/ / data/，双击 wb2api-tray.exe"
