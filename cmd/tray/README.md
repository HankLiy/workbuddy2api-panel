# wb2api-tray — Windows 托盘启动器

独立二进制（`wb2api-tray.exe`），与 `wb2api.exe` 放同一目录使用。**不 import 本仓库任何
internal 包**，只通过孵化子进程管理网关——这是本 fork「低维护同步上游」策略的一部分：
上游任何更新都不会与 `cmd/tray` 产生冲突。

## 功能

- **隐藏运行**：以 `CREATE_NO_WINDOW` 孵化 `wb2api.exe`，不再有控制台黑窗口
- **托盘菜单**：状态显示 / 打开面板（自动从 config.json 读 `listen` 拼地址）/ 查看日志 /
  重启服务 / 开机自启（HKCU Run 键，免管理员）/ 退出
- **状态图标**：蓝 = 运行中，橙 = 异常退出等待自动重启，灰 = 已停止
- **崩溃自愈**：子进程异常退出按指数退避（2s → 60s 封顶）自动重启；运行超 30 秒视为稳定，
  退避计数清零
- **优雅停止**：先向子进程发 `CTRL_BREAK`（网关可完成状态落盘），4 秒未退出才强杀
- **日志**：子进程 stdout/stderr 落 `logs/wb2api.log`，超 10MB 启动时滚动为 `.1`
- **单实例**：命名互斥体防重复启动

## 构建

```powershell
# 仓库根目录
go build -trimpath -ldflags="-s -w"            -o dist/wb2api.exe      ./cmd/server
go build -trimpath -ldflags="-s -w -H windowsgui" -o dist/wb2api-tray.exe ./cmd/tray
```

或使用 `scripts/build-windows.ps1` 一次构建两个 exe。

## 使用

1. 把 `wb2api.exe`、`wb2api-tray.exe` 放同一目录（连同你的 `config.json` / `auths/` / `data/`）
2. 双击 `wb2api-tray.exe`，托盘出现图标即已启动
3. 托盘菜单勾上「开机自启」即可无人值守

参数：`-exe <wb2api.exe 路径>`（默认同目录）、`-config <配置文件>`（默认 `config.json`，
原样传给网关）。
