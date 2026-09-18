package responses

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// serveResponses：POST /v1/responses 的完整处理流程。
//
// 1. 解析并转换请求为 Chat Completions 形态；
// 2. 重写路径后交给 chat handler（鉴权/池调度/粘性/错误策略全部复用）；
// 3. 按客户端 stream 意图与 chat 面实际响应类型转换出口：
//   - chat 面回 SSE（流式请求）→ 实时改写为 Responses 事件序列；
//   - chat 面回 JSON（非流式聚合）→ 聚合转 response 对象；
//   - 非 200 / 错误 → 原样透传（错误语义由 chat 面的 OpenAI 错误体承载）。
func serveResponses(w http.ResponseWriter, r *http.Request, next http.Handler) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}
	chatBody, ec, err := convertRequest(&req)
	if err != nil {
		if br, ok := err.(*badRequest); ok {
			writeError(w, http.StatusBadRequest, br.msg, "invalid_request_error")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}

	// 重写请求：换路径换 body，其余（含 Authorization、ctx 取消传播）原样保留。
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	r2.URL.RawPath = ""
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	r2.Header.Set("Content-Type", "application/json")

	cw := &captureWriter{w: w, header: http.Header{}, ec: ec, wantStream: req.Stream}
	next.ServeHTTP(cw, r2)
	cw.finish()
}

// captureWriter 拦截 chat handler 的响应：
//   - 200 + text/event-stream → 边收边转换（流式）；
//   - 其余 → 全量缓冲，结束后统一处理（聚合 JSON 转换 / 错误透传）。
type captureWriter struct {
	w      http.ResponseWriter
	header http.Header
	ec     *echo

	wantStream bool
	status     int
	streaming  bool
	stream     *streamConverter
	buf        bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	c.status = status
	ct := c.header.Get("Content-Type")
	if status == http.StatusOK && strings.Contains(ct, "text/event-stream") {
		c.streaming = true
		dst := c.w.Header()
		dst.Set("Content-Type", "text/event-stream")
		dst.Set("Cache-Control", "no-cache")
		dst.Set("Connection", "keep-alive")
		dst.Set("X-Accel-Buffering", "no") // 反代后也不缓冲
		c.w.WriteHeader(http.StatusOK)
		c.stream = newStreamConverter(c.w, c.ec)
		c.stream.start()
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.streaming {
		c.stream.feed(p)
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Flush 实现 http.Flusher：chat 面每帧都会 Flush，这里只需透传——
// 转换后的事件在 stream.send 里已逐条 Flush，帧未完整时无事可做。
func (c *captureWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok && c.streaming {
		f.Flush()
	}
}

// finish 在 chat handler 返回后收尾。
func (c *captureWriter) finish() {
	if c.streaming {
		c.stream.finish()
		if f, ok := c.w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	body := c.buf.Bytes()
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}

	// 错误透传：chat 面的 OpenAI 错误体原样回（Responses 客户端按 error.message 展示）。
	if status != http.StatusOK {
		copyHeader(c.w.Header(), c.header)
		c.w.WriteHeader(status)
		_, _ = c.w.Write(body)
		return
	}

	// 非流式聚合 JSON → response 对象。
	var completion map[string]any
	if err := json.Unmarshal(body, &completion); err != nil {
		writeError(c.w, http.StatusBadGateway, "upstream returned a non-JSON response", "api_error")
		return
	}
	resp := completionToResponse(completion, c.ec)
	dst := c.w.Header()
	dst.Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	out, _ := json.Marshal(resp)
	_, _ = c.w.Write(out)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
	_, _ = w.Write(out)
}
