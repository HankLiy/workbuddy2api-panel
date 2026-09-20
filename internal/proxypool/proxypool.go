// Package proxypool 管理「出口代理池」：给每个本地代理端口/节点起一个代号（code），
// 账号在 auth 文件的 proxy 键里写代号即可指定出口（也可直接写 URL，向后兼容）。
//
// 设计目标：一号一出口 IP，降低多账号同 IP 被关联封号的风险。池子本身只存数据，
// 不发起任何网络请求（连通性/质量探测在 upstream.ProbeProxy，由面板按需调用）。
package proxypool

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Entry 单个代理池条目。代号全局唯一（大小写不敏感），供账号 auth 文件引用。
type Entry struct {
	Code    string `json:"code"`              // 代号，如 HK01（字母/数字/下划线/连字符，1-32 位）
	URL     string `json:"url"`               // 代理地址，如 socks5://127.0.0.1:11001
	Note    string `json:"note,omitempty"`    // 备注，如「香港住宅」
	Enabled bool   `json:"enabled"`           // 停用后引用它的账号回落全局/直连
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Validate 校验条目集合：代号非空、格式合法、大小写不敏感唯一；地址非空。
// 返回首个错误的用户可读描述（面板直接展示）。
func Validate(entries []Entry) error {
	seen := make(map[string]string, len(entries))
	for i, e := range entries {
		code := strings.TrimSpace(e.Code)
		if code == "" {
			return fmt.Errorf("第 %d 条：代号不能为空", i+1)
		}
		if !codeRe.MatchString(code) {
			return fmt.Errorf("代号 %q 非法：只允许字母/数字/下划线/连字符，长度 1-32", code)
		}
		key := strings.ToLower(code)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("代号 %q 与 %q 重复（大小写不敏感）", code, prev)
		}
		seen[key] = code
		if strings.TrimSpace(e.URL) == "" {
			return fmt.Errorf("代号 %q：代理地址不能为空", code)
		}
	}
	return nil
}

// Pool 运行期代理池：代号 → 条目。并发安全，支持整体热替换（Replace）。
type Pool struct {
	mu      sync.RWMutex
	entries []Entry
	byCode  map[string]Entry
}

// New 由条目构建池（会拷贝一份，调用方后续改动不影响池内）。
func New(entries []Entry) *Pool {
	p := &Pool{}
	p.Replace(entries)
	return p
}

// Replace 原子替换全部条目（面板保存后热生效）。大小写不敏感的代号索引。
func (p *Pool) Replace(entries []Entry) {
	cp := make([]Entry, len(entries))
	copy(cp, entries)
	byCode := make(map[string]Entry, len(cp))
	for _, e := range cp {
		byCode[strings.ToLower(strings.TrimSpace(e.Code))] = e
	}
	p.mu.Lock()
	p.entries = cp
	p.byCode = byCode
	p.mu.Unlock()
}

// Lookup 按代号查（大小写不敏感）。found=true 表示命中了某代号；enabled 表示该条目启用。
// 调用方据此决定：命中且启用 → 用 url；命中但停用 → 不使用（回落外层策略）；未命中 →
// 说明传入的不是代号（可能是裸 URL），由调用方自行按 URL 处理。
func (p *Pool) Lookup(code string) (url string, found, enabled bool) {
	if p == nil {
		return "", false, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byCode[strings.ToLower(strings.TrimSpace(code))]
	if !ok {
		return "", false, false
	}
	return e.URL, true, e.Enabled
}

// Entries 返回条目快照（拷贝，供面板展示）。
func (p *Pool) Entries() []Entry {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	cp := make([]Entry, len(p.entries))
	copy(cp, p.entries)
	return cp
}

// AccountMap 账号 → 代理引用（代号或 URL）的并发映射，对应 config.json 的
// account_proxies 段。绑定写在配置文件里（而非散在 auths/*.json），一处可查。
type AccountMap struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewAccountMap 由 uid→引用 映射构建（会拷贝）。
func NewAccountMap(m map[string]string) *AccountMap {
	a := &AccountMap{}
	a.Replace(m)
	return a
}

// Replace 原子替换全部绑定。
func (a *AccountMap) Replace(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	a.mu.Lock()
	a.m = cp
	a.mu.Unlock()
}

// ProxyRef 返回账号绑定的代理引用（空 = 未绑定）。实现 upstream.AccountProxyResolver。
func (a *AccountMap) ProxyRef(uid string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.m[uid]
}

// Snapshot 返回绑定快照（拷贝）。
func (a *AccountMap) Snapshot() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cp := make(map[string]string, len(a.m))
	for k, v := range a.m {
		cp[k] = v
	}
	return cp
}
