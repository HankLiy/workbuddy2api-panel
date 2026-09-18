//go:build windows

// wb2api-tray：workbuddy2api-panel 的 Windows 托盘启动器（独立二进制）。
//
// 设计约束（为了 fork 长期低维护）：
//   - 不 import 本仓库任何 internal 包，只通过「孵化 wb2api.exe 子进程 + HTTP」交互，
//     上游任意更新都不会与本目录产生冲突；
//   - 以 CREATE_NO_WINDOW 隐藏窗口方式孵化 wb2api.exe，日志落 logs/wb2api.log；
//   - 托盘菜单：打开面板 / 查看日志 / 重启 / 开机自启（注册表 Run 键）/ 退出；
//   - 子进程异常退出自动重启（指数退避），图标变色提示状态（蓝=运行，橙=等待重启，灰=停止）。
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// SysProcAttr CreationFlags：子进程隐藏控制台窗口 + 独立进程组（后者是发 CTRL_BREAK 的前提）。
const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
	ctrlBreakEvent        = 1
)

const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "wb2api-tray"
	mutexName    = `Local\wb2api-tray-single-instance`

	maxLogBytes  = 10 << 20 // wb2api.log 超过 10MB 启动时滚动为 .1
	gracePeriod  = 4 * time.Second
	stableUptime = 30 * time.Second // 运行超过该时长视为稳定，崩溃退避计数清零
)

var (
	flagExe    = flag.String("exe", "", "wb2api.exe 路径（默认与托盘程序同目录）")
	flagConfig = flag.String("config", "config.json", "传给 wb2api.exe 的 -config")
)

func main() {
	flag.Parse()

	// 单实例：重复启动直接退出（托盘已有实例在管）。
	mu, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(mutexName))
	if err == nil {
		defer windows.CloseHandle(mu)
		if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
			return
		}
	}

	exePath := *flagExe
	if exePath == "" {
		self, _ := os.Executable()
		exePath = filepath.Join(filepath.Dir(self), "wb2api.exe")
	}
	exePath, _ = filepath.Abs(exePath)

	m := newManager(exePath, *flagConfig)
	systray.Run(func() { onReady(m) }, func() { m.stop() })
}

// manager 管理 wb2api.exe 子进程的生命周期。
type manager struct {
	exePath string
	cfgPath string
	dir     string
	logPath string

	mu        sync.Mutex
	cmd       *exec.Cmd
	exitCh    chan struct{} // 由 watch 在子进程退出后关闭
	startedAt time.Time
	stopping  bool // 用户主动停止/退出，异常重启逻辑不应介入
	crashes   int  // 连续快速崩溃次数（驱动退避）
}

func newManager(exePath, cfgPath string) *manager {
	dir := filepath.Dir(exePath)
	return &manager{
		exePath: exePath,
		cfgPath: cfgPath,
		dir:     dir,
		logPath: filepath.Join(dir, "logs", "wb2api.log"),
	}
}

func (m *manager) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		return nil
	}
	if _, err := os.Stat(m.exePath); err != nil {
		return fmt.Errorf("找不到 wb2api.exe：%s", m.exePath)
	}

	f, err := m.openLog()
	if err != nil {
		return err
	}

	cmd := exec.Command(m.exePath, "-config", m.cfgPath)
	cmd.Dir = m.dir // config/auths/data 均按相对路径解析到 exe 目录
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow}
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}

	m.cmd = cmd
	m.exitCh = make(chan struct{})
	m.startedAt = time.Now()
	m.stopping = false
	exitCh := m.exitCh
	go m.watch(cmd, f, exitCh)
	setState(stateRunning)
	updateStatus(fmt.Sprintf("运行中 (pid %d)", cmd.Process.Pid))
	return nil
}

// watch 等待子进程退出；非用户主动停止时按指数退避自动重启。
func (m *manager) watch(cmd *exec.Cmd, f *os.File, exitCh chan struct{}) {
	err := cmd.Wait()
	f.Close()
	close(exitCh)

	m.mu.Lock()
	if m.cmd == cmd {
		m.cmd = nil
	}
	uptime := time.Since(m.startedAt)
	stopping := m.stopping
	if uptime >= stableUptime {
		m.crashes = 0
	}
	m.crashes++
	delay := time.Duration(math.Min(
		float64(2*time.Second)*math.Pow(2, float64(m.crashes-1)),
		float64(60*time.Second),
	))
	m.mu.Unlock()

	if stopping {
		return
	}
	setState(stateRestarting)
	updateStatus(fmt.Sprintf("已退出（%v），%s 后自动重启", err, delay.Round(time.Second)))
	time.Sleep(delay)
	if err := m.start(); err != nil {
		setState(stateStopped)
		updateStatus("重启失败：" + err.Error())
	}
}

// stop 优雅停止：先发 CTRL_BREAK（server 监听 os/signal，可完成状态落盘），超时强杀。
func (m *manager) stop() {
	m.mu.Lock()
	m.stopping = true
	cmd := m.cmd
	exitCh := m.exitCh
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	updateStatus("正在停止…")
	_ = windows.GenerateConsoleCtrlEvent(ctrlBreakEvent, uint32(cmd.Process.Pid))
	select {
	case <-exitCh:
	case <-time.After(gracePeriod):
		_ = cmd.Process.Kill()
		<-exitCh
	}
	setState(stateStopped)
	updateStatus("已停止")
}

func (m *manager) restart() {
	m.stop()
	m.mu.Lock()
	m.stopping = false // 复位，让这次 start 之后的异常仍走自动重启
	m.mu.Unlock()
	if err := m.start(); err != nil {
		setState(stateStopped)
		updateStatus("启动失败：" + err.Error())
	}
}

// openLog 打开（必要时滚动）日志文件。
func (m *manager) openLog() (*os.File, error) {
	if st, err := os.Stat(m.logPath); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(m.logPath, m.logPath+".1") // 只保留一代，覆盖旧的
	}
	if err := os.MkdirAll(filepath.Dir(m.logPath), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(m.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// panelURL 从 config.json 读 listen，拼面板地址（0.0.0.0/:port 归一到 127.0.0.1）。
func (m *manager) panelURL() string {
	listen := ":7863"
	if data, err := os.ReadFile(filepath.Join(m.dir, m.cfgPath)); err == nil {
		var c struct {
			Listen string `json:"listen"`
		}
		if json.Unmarshal(data, &c) == nil && c.Listen != "" {
			listen = c.Listen
		}
	}
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	host = strings.ReplaceAll(host, "0.0.0.0", "127.0.0.1")
	return "http://" + host + "/panel/"
}

// ---------- 托盘 UI ----------

var statusItem *systray.MenuItem

// 托盘状态 → 图标颜色。
type trayState int

const (
	stateRunning    trayState = iota // 蓝：服务运行中
	stateRestarting                  // 橙：异常退出，等待自动重启
	stateStopped                     // 灰：已停止 / 启动失败
)

func setState(s trayState) {
	switch s {
	case stateRunning:
		systray.SetIcon(makeIcon(0x4F, 0x8C, 0xFF)) // #4F8CFF
	case stateRestarting:
		systray.SetIcon(makeIcon(0xFF, 0xA0, 0x3C)) // 橙
	case stateStopped:
		systray.SetIcon(makeIcon(0x99, 0x99, 0x99)) // 灰
	}
}

func updateStatus(s string) {
	systray.SetTooltip("wb2api: " + s)
	if statusItem != nil {
		statusItem.SetTitle("状态：" + s)
	}
}

func onReady(m *manager) {
	setState(stateStopped)
	systray.SetTooltip("wb2api: 启动中…")

	statusItem = systray.AddMenuItem("状态：启动中…", "当前状态")
	statusItem.Disable()
	systray.AddSeparator()
	mPanel := systray.AddMenuItem("打开面板", "浏览器打开 Web 管理面板")
	mLog := systray.AddMenuItem("查看日志", "用默认程序打开 logs/wb2api.log")
	mRestart := systray.AddMenuItem("重启服务", "停止并重新启动 wb2api.exe")
	systray.AddSeparator()
	mAuto := systray.AddMenuItemCheckbox("开机自启", "登录 Windows 时自动启动本托盘程序", autostartEnabled())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止 wb2api 并退出托盘")

	if err := m.start(); err != nil {
		setState(stateStopped)
		updateStatus("启动失败：找不到 wb2api.exe 或配置错误")
	}

	for {
		select {
		case <-mPanel.ClickedCh:
			openExternal(m.panelURL())
		case <-mLog.ClickedCh:
			openExternal(m.logPath)
		case <-mRestart.ClickedCh:
			go m.restart()
		case <-mAuto.ClickedCh:
			if autostartEnabled() {
				_ = setAutostart(false)
				mAuto.Uncheck()
			} else {
				if err := setAutostart(true); err == nil {
					mAuto.Check()
				}
			}
		case <-mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// openExternal 用系统默认程序打开 URL / 文件（rundll32 无需引入 shell 执行风险）。
func openExternal(target string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}

// ---------- 开机自启（HKCU Run 键，免管理员） ----------

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err = k.DeleteValue(runValueName)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.Abs(self)
	return k.SetStringValue(runValueName, `"`+self+`"`)
}

// ---------- 托盘图标（运行时生成 32x32 ICO，零外部资源） ----------

// makeIcon 生成一个圆环 ICO（BGRA 位图格式，32bpp 带 alpha，AND 掩码全 0）。
// r/g/b 为内圆颜色，外环自动取 75% 亮度。
func makeIcon(r, g, b byte) []byte {
	const size = 32
	px := make([]byte, size*size*4)
	cx, cy, rad := 15.5, 15.5, 14.5
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			d := math.Sqrt(dx*dx + dy*dy)
			i := (y*size + x) * 4
			switch {
			case d > rad:
				// 透明
			case d > rad-4: // 外环：75% 亮度
				px[i+0], px[i+1], px[i+2], px[i+3] = b*3/4, g*3/4, r*3/4, 0xFF
			default: // 内圆
				px[i+0], px[i+1], px[i+2], px[i+3] = b, g, r, 0xFF
			}
		}
	}
	andMask := make([]byte, size*size/8)

	var buf bytes.Buffer
	// ICONDIR
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // count
	// ICONDIRENTRY
	imgSize := uint32(40 + len(px) + len(andMask))
	buf.WriteByte(size) // width
	buf.WriteByte(size) // height
	buf.WriteByte(0)    // palette
	buf.WriteByte(0)    // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))          // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))         // bit count
	_ = binary.Write(&buf, binary.LittleEndian, imgSize)            // bytes in res
	_ = binary.Write(&buf, binary.LittleEndian, uint32(6+16))       // image offset
	// BITMAPINFOHEADER（高度为 2 倍：XOR + AND）
	_ = binary.Write(&buf, binary.LittleEndian, uint32(40))         // biSize
	_ = binary.Write(&buf, binary.LittleEndian, int32(size))        // width
	_ = binary.Write(&buf, binary.LittleEndian, int32(size*2))      // height (xor+and)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))          // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))         // bitCount
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))          // compression
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(px)+len(andMask)))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))           // x ppm
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))           // y ppm
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))          // colors used
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))          // important colors
	// BMP 像素自下而上：翻转行序写入
	for y := size - 1; y >= 0; y-- {
		buf.Write(px[y*size*4 : (y+1)*size*4])
	}
	buf.Write(andMask)
	return buf.Bytes()
}
