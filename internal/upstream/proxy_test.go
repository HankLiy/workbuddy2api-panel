package upstream

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"127.0.0.1:1080", "http://127.0.0.1:1080", true},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890", true},
		{"https://proxy.local:8443", "https://proxy.local:8443", true},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", true},
		// socks5h 归一为 socks5（Go 的 socks5 dialer 默认在代理端解析域名）。
		{"socks5h://127.0.0.1:1080", "socks5://127.0.0.1:1080", true},
		{"http://user:pass@127.0.0.1:8080", "http://user:pass@127.0.0.1:8080", true},
		// 非法：不支持的 scheme / 缺 host。
		{"ftp://127.0.0.1:21", "", false},
		{"http://", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeProxy(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("normalizeProxy(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// fakeProxyPool 测试用代号解析器。
type fakeProxyPool map[string]struct {
	url     string
	enabled bool
}

func (f fakeProxyPool) Lookup(code string) (string, bool, bool) {
	e, ok := f[code]
	if !ok {
		return "", false, false
	}
	return e.url, true, e.enabled
}

// fakeAccounts 测试用账号→引用映射（实现 upstream.AccountProxyResolver）。
type fakeAccounts map[string]string

func (f fakeAccounts) ProxyRef(uid string) string { return f[uid] }

func proxyOf(t *testing.T, cl *http.Client) string {
	t.Helper()
	tr, ok := cl.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", cl.Transport)
	}
	if tr.Proxy == nil {
		return ""
	}
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "copilot.tencent.com"}})
	if err != nil || u == nil {
		return ""
	}
	return u.String()
}

// TestProxyResolve 生效代理优先级：账号绑定（代号/URL）> 全局；代号命中启用→url、
// 命中停用→回落、账号侧裸 host:port→不猜测。
func TestProxyResolve(t *testing.T) {
	c := New()
	c.Proxy = "http://global:8080"
	c.ProxyPool = fakeProxyPool{
		"HK01": {url: "socks5://127.0.0.1:11001", enabled: true},
		"JP02": {url: "socks5://127.0.0.1:11002", enabled: false},
	}

	// 命中启用代号 → 池内 url。
	c.AccountProxy = fakeAccounts{"u1": "HK01", "u2": "JP02", "u3": "socks5://127.0.0.1:9", "u4": "127.0.0.1:1080"}
	if got := c.proxyFor(&auth.Auth{UID: "u1"}); got != "socks5://127.0.0.1:11001" {
		t.Errorf("u1(HK01) → %q, want socks5://127.0.0.1:11001", got)
	}
	// 命中停用代号 → 回落全局。
	if got := c.proxyFor(&auth.Auth{UID: "u2"}); got != "http://global:8080" {
		t.Errorf("u2(JP02 disabled) → %q, want global", got)
	}
	// 账号侧带 scheme 的裸 URL → 直接用。
	if got := c.proxyFor(&auth.Auth{UID: "u3"}); got != "socks5://127.0.0.1:9" {
		t.Errorf("u3(URL) → %q, want socks5://127.0.0.1:9", got)
	}
	// 账号侧无 scheme 且非代号（疑似错字）→ 不猜测，回落全局。
	if got := c.proxyFor(&auth.Auth{UID: "u4"}); got != "http://global:8080" {
		t.Errorf("u4(bare host typo) → %q, want global fallback", got)
	}
	// 未绑定 → 全局。
	if got := c.proxyFor(&auth.Auth{UID: "none"}); got != "http://global:8080" {
		t.Errorf("unbound → %q, want global", got)
	}
	// 全局也为代号 → 解析。
	c.AccountProxy = nil
	c.Proxy = "HK01"
	if got := c.proxyFor(&auth.Auth{UID: "u"}); got != "socks5://127.0.0.1:11001" {
		t.Errorf("global code → %q, want socks5://127.0.0.1:11001", got)
	}
}

func TestClientSelectionByProxy(t *testing.T) {
	c := New()
	// 无绑定、无全局 → 直连复用共享 c.HTTP / c.ChatHTTP。
	if got := c.httpFor(&auth.Auth{UID: "u"}); got != c.HTTP {
		t.Error("no proxy → should reuse shared c.HTTP")
	}
	if got := c.chatFor(&auth.Auth{UID: "u"}); got != c.ChatHTTP {
		t.Error("no proxy → chat should reuse shared c.ChatHTTP")
	}

	c.AccountProxy = fakeAccounts{
		"u1": "socks5://127.0.0.1:1080", // 同代理（u2）
		"u2": "socks5://127.0.0.1:1080",
		"u3": "http://127.0.0.1:7890", // 异代理
	}
	cl1 := c.httpFor(&auth.Auth{UID: "u1"})
	if cl1 == c.HTTP {
		t.Fatal("proxied account should not reuse direct c.HTTP")
	}
	if got := proxyOf(t, cl1); got != "socks5://127.0.0.1:1080" {
		t.Errorf("transport proxy = %q, want socks5://127.0.0.1:1080", got)
	}
	if c.httpFor(&auth.Auth{UID: "u2"}) != cl1 {
		t.Error("same proxy should reuse the same client/transport")
	}
	if c.httpFor(&auth.Auth{UID: "u3"}) == cl1 {
		t.Error("different proxy must not share client")
	}
	if got := proxyOf(t, c.httpFor(&auth.Auth{UID: "u3"})); got != "http://127.0.0.1:7890" {
		t.Errorf("transport proxy = %q, want http://127.0.0.1:7890", got)
	}

	// chat client：Timeout=0，独立对象但与 JSON client 共享同一代理的 transport。
	chat1 := c.chatFor(&auth.Auth{UID: "u1"})
	if chat1.Timeout != 0 {
		t.Errorf("chat client Timeout = %v, want 0", chat1.Timeout)
	}
	if chat1 == cl1 {
		t.Error("chat and json clients should be distinct objects")
	}
	if chat1.Transport != cl1.Transport {
		t.Error("chat and json clients of same proxy should share the transport (one pool)")
	}
}

func TestClientSelectionInvalidProxyFallsBackToDirect(t *testing.T) {
	c := New()
	c.AccountProxy = fakeAccounts{"u1": "ftp://nope:21"}
	if got := c.httpFor(&auth.Auth{UID: "u1"}); got != c.HTTP {
		t.Error("invalid proxy should fall back to direct c.HTTP")
	}
}

// TestProxyStats 运行期按代理引用累计尝试数与传输层错误数。
func TestProxyStats(t *testing.T) {
	// 失败：账号绑定到一个无监听的 socks5 → Do 必报传输层错误。
	c := New()
	c.AccountProxy = fakeAccounts{"u1": "socks5://127.0.0.1:1"}
	a := &auth.Auth{UID: "u1"}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if _, err := c.doWithStats(c.httpFor(a), req, a); err == nil {
		t.Fatal("expected transport error")
	}
	stats := c.ProxyStats()
	if len(stats) != 1 || stats[0].Ref != "socks5://127.0.0.1:1" || stats[0].Total != 1 || stats[0].TransportErr != 1 || stats[0].LastErr == "" {
		t.Fatalf("stats=%+v, want ref=socks5://127.0.0.1:1 total=1 err=1 lastErr set", stats)
	}

	// 成功：经本地 HTTP 代理命中 200 → total 增、err 不增。
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	c.AccountProxy = fakeAccounts{"u1": proxy.URL}
	req2, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	resp, err := c.doWithStats(c.httpFor(a), req2, a)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	resp.Body.Close()
	for _, s := range c.ProxyStats() {
		if s.Ref == proxy.URL {
			if s.Total != 1 || s.TransportErr != 0 {
				t.Errorf("success stats=%+v, want total=1 err=0", s)
			}
			return
		}
	}
	t.Errorf("no stats for proxy %q: %+v", proxy.URL, c.ProxyStats())
}

// TestProxyTransportActuallyRoutes 端到端：账号代理真的把请求送进了代理（HTTP 代理收到
// 绝对 URI 形式的请求），目标直连未被触碰。
func TestProxyTransportActuallyRoutes(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.URL.Host == "" {
			t.Errorf("proxy request not in absolute-URI form: %q", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("target reached directly — request bypassed the proxy")
	}))
	defer target.Close()

	c := New()
	c.AccountProxy = fakeAccounts{"u1": proxy.URL}
	req, err := http.NewRequest(http.MethodGet, target.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.httpFor(&auth.Auth{UID: "u1"}).Do(req)
	if err != nil {
		t.Fatalf("Do via proxy: %v", err)
	}
	_ = resp.Body.Close()
	if proxyHits != 1 {
		t.Errorf("proxy hits = %d, want 1", proxyHits)
	}
}
