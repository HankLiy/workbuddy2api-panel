package panel

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

// TestAccountProxyWritesConfig 面板设置账号代理：走 SetAccountProxy（写 config
// account_proxies），不再落 auths/*.json；空串清除。
func TestAccountProxyWritesConfig(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-utest.json")
	authBody := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":9999999999},"account":{"uid":"utest"}}`
	if err := os.WriteFile(fp, []byte(authBody), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse([]byte(authBody))
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = fp

	pl := pool.New(filepath.Join(dir, "state.json"))
	pl.Add(a)

	am := proxypool.NewAccountMap(nil)
	pn := New(Config{
		Version:          "test",
		APIKey:           "test-key",
		Pool:             pl,
		ProxyPoolEntries: func() []proxypool.Entry { return []proxypool.Entry{{Code: "HK01", URL: "socks5://127.0.0.1:11001", Enabled: true}} },
		AccountProxyOf:   am.ProxyRef,
		SetAccountProxy: func(uid, ref string) error {
			m := am.Snapshot()
			if strings.TrimSpace(ref) == "" {
				delete(m, uid)
			} else {
				m[uid] = ref
			}
			am.Replace(m)
			return nil
		},
	})

	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/utest/proxy", strings.NewReader(body))
		req.SetPathValue("uid", "utest")
		rec := httptest.NewRecorder()
		pn.accountProxy(rec, req)
		return rec
	}

	if rec := do(`{"proxy":"HK01"}`); rec.Code != 200 {
		t.Fatalf("set status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ref := am.ProxyRef("utest"); ref != "HK01" {
		t.Errorf("bound ref=%q want HK01", ref)
	}
	// auth 文件不得被写入代理键（配置集中在 config.json）。
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), "proxy") {
		t.Errorf("auth file must not contain proxy key, got: %s", raw)
	}

	if rec := do(`{"proxy":""}`); rec.Code != 200 {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ref := am.ProxyRef("utest"); ref != "" {
		t.Errorf("cleared ref=%q want empty", ref)
	}
}
