# FORK 维护说明（HankLiy 分支）

本 fork 在 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
之上叠加两个功能，设计目标是**同步上游时零冲突**。

## 与上游的差异（全部改动）

| 改动 | 位置 | 与上游代码的接触面 |
|---|---|---|
| Windows 托盘启动器 | `cmd/tray/`（新目录） | 零接触：不 import 任何 internal 包，只孵化 wb2api.exe 子进程 |
| Responses API 支持 | `internal/responses/`（新目录） | `cmd/server/main.go` 1 行（`responses.Wrap(h)`）+ 1 行 import |
| 构建脚本 | `scripts/build-windows.ps1` | 零接触 |
| 依赖 | `go.mod` / `go.sum` 增加 getlantern/systray 等 | 合并时用 `go mod tidy` 一键解决 |

## 分支布局

- `main`：跟踪上游，**不在上面直接开发**
- `mine`：你的功能分支（当前部署分支）

## 同步上游（想要上游新功能时）

```bash
git fetch upstream
git checkout main && git merge --ff-only upstream/main && git push origin main
git checkout mine && git rebase main
# 唯一可能冲突的是 go.mod/go.sum → go mod tidy 后 git rebase --continue
git push --force-with-lease origin mine
```

或在 GitHub 网页上点 **Sync fork**，然后本地 `mine` 上 `git rebase origin/main`。

平时不需要任何维护；建议只在想要上游的某个新功能时才同步。

## 发布构建（Windows）

```powershell
scripts/build-windows.ps1
# 产出 dist/wb2api.exe + dist/wb2api-tray.exe
```

部署：两个 exe 放同一目录，连同 `config.json` / `auths/` / `data/`（见上游 README），
双击 `wb2api-tray.exe`。托盘菜单勾「开机自启」即无人值守。

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
