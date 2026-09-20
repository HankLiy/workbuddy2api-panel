// proxypool.go 面板「IP 池」接口：列出条目 + 绑定账号/健康聚合、保存条目、测试节点。
//
// 存储复用 config.json 的 proxy_pool 段：保存走 main 注入的 SaveConfig 闭包
// （校验 → 落盘 → 热替换池），与配置页同一套原子写+热生效管线，不另起状态文件。
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// getProxyPool 返回 IP 池条目，并附带每个代号绑定的账号与健康聚合（成功/错误/冷却）。
func (p *Panel) getProxyPool(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ProxyPoolEntries == nil {
		writeErr(w, http.StatusNotImplemented, "proxy pool not available")
		return
	}
	entries := p.cfg.ProxyPoolEntries()

	// 账号聚合：按账号的代理引用（代号或 URL，大小写不敏感）归到对应条目。
	type acctRow struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Cooling  bool   `json:"cooling"`
		Disabled bool   `json:"disabled"`
		Success  int64  `json:"success"`
		Err      int64  `json:"err"`
	}
	bound := make(map[string][]acctRow, len(entries))
	if p.cfg.Pool != nil && p.cfg.AccountProxyOf != nil {
		for _, st := range p.cfg.Pool.List() {
			ref := strings.ToLower(strings.TrimSpace(p.cfg.AccountProxyOf(st.UID)))
			if ref == "" {
				continue
			}
			bound[ref] = append(bound[ref], acctRow{
				UID: st.UID, Nickname: st.Nickname, Cooling: st.Cooling,
				Disabled: st.Disabled, Success: st.SuccessCount, Err: st.ErrTotal,
			})
		}
	}

	// 运行期链路统计（按代理引用；upstream 层累计的传输层尝试/错误）。
	runtime := map[string]upstream.ProxyStatSnapshot{}
	if p.cfg.Upstream != nil {
		for _, s := range p.cfg.Upstream.ProxyStats() {
			runtime[strings.ToLower(strings.TrimSpace(s.Ref))] = s
		}
	}

	out := make([]map[string]any, 0, len(entries))
	matched := make(map[string]bool, len(entries))
	for _, e := range entries {
		accts := bound[strings.ToLower(strings.TrimSpace(e.Code))]
		var success, errs int64
		for _, a := range accts {
			success += a.Success
			errs += a.Err
		}
		// 运行期统计：按代号匹配；账号若直接绑 URL，则按 URL 匹配。
		key := strings.ToLower(strings.TrimSpace(e.Code))
		rt := runtime[key]
		if rt.Ref == "" {
			key = strings.ToLower(strings.TrimSpace(e.URL))
			rt = runtime[key]
		}
		if rt.Ref != "" {
			matched[key] = true
		}
		out = append(out, map[string]any{
			"code": e.Code, "url": e.URL, "note": e.Note, "enabled": e.Enabled,
			"accounts": accts, "success": success, "err": errs,
			"rt_total": rt.Total, "rt_err": rt.TransportErr,
			"rt_last_error": rt.LastErr, "rt_last_error_at": rt.LastErrAt,
		})
	}

	// 未归入任何条目的引用（全局 proxy 或直连 ""）：单独列出，别把它们藏起来。
	other := make([]map[string]any, 0)
	for k, s := range runtime {
		if matched[k] {
			continue
		}
		other = append(other, map[string]any{
			"ref": s.Ref, "total": s.Total, "transport_err": s.TransportErr,
			"last_error": s.LastErr, "last_error_at": s.LastErrAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entries": out, "other": other})
}

// saveProxyPool 保存 IP 池条目：body {"entries":[{code,url,note,enabled}...]}。
// 经 SaveConfig 闭包校验+落盘 config.json 的 proxy_pool，并热替换运行期池。
func (p *Panel) saveProxyPool(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var body struct {
		Entries []proxypool.Entry `json:"entries"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// 复用一个"仅含 proxy_pool 键"的配置补丁：main 的 SaveConfig 深合并进现有配置，
	// 其余键原样保留，因此这里只提交 proxy_pool 即可。
	patch, err := json.Marshal(map[string]any{"proxy_pool": body.Entries})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
}

// testProxyPool 经指定代理地址访问第三方回显服务，返回出口 IP 与延迟（节点质量）。
// body {"url":"socks5://127.0.0.1:11001"}。测试失败也回 200 + ok:false，便于前端展示。
func (p *Panel) testProxyPool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "url required")
		return
	}
	probe, err := upstream.ProbeProxy(body.URL, 12*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "latency_ms": probe.LatencyMS})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "latency_ms": probe.LatencyMS,
		"ip": probe.IP, "country": probe.Country, "city": probe.City, "org": probe.Org,
	})
}
