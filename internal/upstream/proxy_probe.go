package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ProxyProbe 代理连通性/出口探测结果（面板「测试」按钮展示）。
type ProxyProbe struct {
	LatencyMS int64  `json:"latency_ms"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	City      string `json:"city,omitempty"`
	Org       string `json:"org,omitempty"`
}

// ProbeProxy 经指定代理访问第三方回显服务（ipinfo.io/json），返回出口 IP、归属与延迟。
// 仅用于面板评估节点质量，不参与任何路由决策。代理地址同网关口径（http/https/socks5，
// 无 scheme 默认 http://）。
func ProbeProxy(rawURL string, timeout time.Duration) (ProxyProbe, error) {
	key, ok := normalizeProxy(rawURL)
	if !ok {
		return ProxyProbe{}, fmt.Errorf("无效代理地址：%q", rawURL)
	}
	u, err := url.Parse(key)
	if err != nil {
		return ProxyProbe{}, err
	}
	tr := newTransport()
	tr.Proxy = http.ProxyURL(u)
	cl := &http.Client{Timeout: timeout, Transport: tr}

	req, err := http.NewRequest(http.MethodGet, "https://ipinfo.io/json", nil)
	if err != nil {
		return ProxyProbe{}, err
	}
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return ProxyProbe{}, err
	}
	defer resp.Body.Close()
	lat := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		return ProxyProbe{LatencyMS: lat}, fmt.Errorf("回显服务返回 HTTP %d", resp.StatusCode)
	}
	var meta struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Country string `json:"country"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&meta); err != nil {
		return ProxyProbe{LatencyMS: lat}, fmt.Errorf("解析回显响应: %w", err)
	}
	return ProxyProbe{LatencyMS: lat, IP: meta.IP, Country: meta.Country, City: meta.City, Org: meta.Org}, nil
}
