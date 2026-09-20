package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

// TestProxyPoolListAndSave 面板 IP 池：GET 返回条目+绑定账号聚合；POST 经 SaveConfig
// 闭包持久化（只提交 proxy_pool 补丁）。绑定来自 config account_proxies。
func TestProxyPoolListAndSave(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-utest.json")
	b := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":9999999999},"account":{"uid":"utest","nickname":"测试号"}}`)
	if err := os.WriteFile(fp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = fp

	pl := pool.New(filepath.Join(dir, "state.json"))
	pl.Add(a)

	am := proxypool.NewAccountMap(map[string]string{"utest": "HK01"})
	entries := []proxypool.Entry{{Code: "HK01", URL: "socks5://127.0.0.1:11001", Enabled: true}}
	var savedPatch []byte
	pn := New(Config{
		Version:          "test",
		APIKey:           "test-key",
		Pool:             pl,
		ProxyPoolEntries: func() []proxypool.Entry { return entries },
		AccountProxyOf:   am.ProxyRef,
		SetAccountProxy:  func(uid, ref string) error { return nil },
		SaveConfig:       func(raw []byte) ([]string, error) { savedPatch = raw; return nil, nil },
	})

	rec := httptest.NewRecorder()
	pn.getProxyPool(rec, httptest.NewRequest("GET", "/panel/api/proxypool", nil))
	if rec.Code != 200 {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []struct {
			Code     string `json:"code"`
			Accounts []struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"accounts"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].Code != "HK01" {
		t.Fatalf("entries=%+v", out.Entries)
	}
	if len(out.Entries[0].Accounts) != 1 || out.Entries[0].Accounts[0].UID != "utest" {
		t.Errorf("bound accounts=%+v, want [utest]", out.Entries[0].Accounts)
	}

	rec2 := httptest.NewRecorder()
	pn.saveProxyPool(rec2, httptest.NewRequest("POST", "/panel/api/proxypool",
		strings.NewReader(`{"entries":[{"code":"JP02","url":"http://127.0.0.1:11002","enabled":false}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("save status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(savedPatch, &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch) != 1 {
		t.Errorf("patch keys=%v, want only proxy_pool", patch)
	}
	var got []proxypool.Entry
	if err := json.Unmarshal(patch["proxy_pool"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Code != "JP02" || got[0].Enabled {
		t.Errorf("saved entries=%+v, want JP02 disabled", got)
	}
}
