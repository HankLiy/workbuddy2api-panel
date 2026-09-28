# FORK 维护说明（HankLiy 分支）

本 fork 在 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
之上叠加两个功能，设计目标是**同步上游时零冲突**。

> 自用 fork，不向上游提 PR：整个仓库只有一条分支 **`main`**，
> 它同时承载「上游最新代码 + 本 fork 的功能」。构建、部署、同步都在 `main` 上进行。

## 与上游的差异（全部改动）

| 改动 | 位置 | 与上游代码的接触面 |
|---|---|---|
| Windows 托盘启动器 | `cmd/tray/`（新目录） | 零接触：不 import 任何 internal 包，只孵化 wb2api.exe 子进程 |
| Responses API 支持 | `internal/responses/`（新目录） | `cmd/server/main.go` 1 行（`responses.Wrap(h)`）+ 1 行 import |
| CI 构建口径偏离上游 | `.github/workflows/go-binaries.yml` | Windows 产物用 `-w`（非上游的 `-s -w`）+ 额外构建并打包 `wb2api-tray.exe` |
| 构建脚本 | `scripts/build-windows.ps1` | 零接触 |
| 依赖 | `go.mod` / `go.sum` 增加 getlantern/systray 等 | 合并时用 `go mod tidy` 一键解决 |

## 分支布局

只有一条分支 `main`，别无其他（`mine` 已废弃/删除）。

- 上游远程：`upstream` = `linguo2625469/workbuddy2api-panel`
- 本 fork 远程：`origin` = `HankLiy/workbuddy2api-panel`
- 本地 `main` 已配置 `upstream`，用 merge 方式吸收上游（不 rebase、不 force-push）

## 同步上游（想要上游新功能时）

```bash
git fetch upstream
git checkout main
git merge upstream/main        # 把上游新提交合进来；仅 go.mod/go.sum 可能冲突 → go mod tidy 后提交
git push origin main
```

也可以直接在 GitHub 网页点 **Sync fork**（它等价于把上游默认分支合并进你的 `main`）。

平时不需要任何维护；建议只在想要上游的某个新功能时才同步。

## 发布构建（Windows）

```powershell
scripts/build-windows.ps1
# 产出 dist/wb2api.exe + dist/wb2api-tray.exe
```

部署：两个 exe 放同一目录，连同 `config.json` / `auths/` / `data/`（见上游 README），
双击 `wb2api-tray.exe`。托盘菜单勾「开机自启」即无人值守。

## CI 与上游的偏离（吸收上游 workflow 时注意）

上游同步带来了 `.github/workflows/go-binaries.yml`（五平台二进制 + Release）与
`docker-ghcr.yml`（多架构镜像）。fork 对它们的处置：

- **`go-binaries.yml`：已改，两处偏离上游**
  1. **补建 `wb2api-tray.exe`**：上游只构建 `./cmd/server`，Release 不含托盘启动器。
     本 fork 在 `matrix.goos == 'windows'` 时额外构建 `./cmd/tray`（`-H windowsgui`），
     并把 Windows zip 打成**四件套**：`wb2api.exe` + `wb2api-tray.exe` + `config.example.json` + `README.md`。
  2. **ldflags 用 `-w` 而非上游的 `-s -w`**：与 `scripts/build-windows.ps1` 同口径。
     大体积 Go 二进制无符号表会被部分杀软（火绒/卡巴）启发式误判并删除（实测 `-s` 被删、`-w` 存活）。
     五个平台统一 `-w` 以保持矩阵一致。

  > 上游那两个 workflow 原本还引用了本仓库不存在的 `HANDOFF` 文档（仅注释文案），已在改写注释时移除。

- **`docker-ghcr.yml`：未改**。它用 `${{ github.repository }}` 推送到 `ghcr.io/<本仓库>`，
  对 fork 自洽（镜像只含 server，tray 是 Windows GUI 本就不该进镜像）。
  注意：上游 `README.md` 里写死的是 `ghcr.io/linguo2625469/...`，fork 下实际镜像名是
  `ghcr.io/HankLiy/workbuddy2api-panel`；README 是上游文件，fork 不修改以免增加同步冲突面。

- **README.md 与现状不符处（已知、不修）**：`README.md` 写「无预编译 release：仓库无 Release / tag」，
  但 CI 现在会建 Release 与 tag；GHCR 镜像名也指向上游。这些属上游文件内容，fork 保持原样。

> 修改 `.github/workflows/go-binaries.yml` 会在下次 `git merge upstream/main` 时与上游改动冲突——
> 冲突通常局限于顶部注释与 build/打包两处 step，按「保留上游新逻辑 + 叠加 fork 的 tray/`-w`」原则手工合并即可。

## Codex CLI 接入

```toml
# config.toml
model_provider = "wb2api"
wire_api = "responses"
base_url = "http://127.0.0.1:7863/v1"   # 按实际 listen 调整
api_key  = "<面板 config.json 里的 api_key>"
```

`/v1/responses` 为无状态实现：不支持 `previous_response_id` / `item_reference`
（请求会直接 400 并说明），多轮对话全量历史放在 `input` 里（Codex CLI 默认即如此）。
