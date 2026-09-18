package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- 请求转换 ----------

func TestConvertStringInput(t *testing.T) {
	var req request
	mustJSON(t, `{"model":"glm-5.2","input":"你好"}`, &req)
	body, ec, err := convertRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	mustJSON(t, string(body), &out)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages=%v", msgs)
	}
	if ec.model != "glm-5.2" {
		t.Fatalf("echo.model=%q", ec.model)
	}
}

func TestConvertInstructionsAndItems(t *testing.T) {
	var req request
	mustJSON(t, `{
		"model":"glm-5.2",
		"instructions":"你是助手",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"天气?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"bj\"}"},
			{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"晴"},
			{"type":"function_call_output","call_id":"call_2","output":[{"type":"output_text","text":"12:00"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"想了一下"}]}
		],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"web_search"}],
		"tool_choice":"auto",
		"reasoning":{"effort":"high"},
		"max_output_tokens":2048,
		"stream":true
	}`, &req)
	body, ec, err := convertRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	mustJSON(t, string(body), &out)

	msgs := out["messages"].([]any)
	// system(instructions) + user + assistant(合并 2 个 tool_calls) + tool + tool
	if len(msgs) != 5 {
		t.Fatalf("messages len=%d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("msgs[0]=%v", msgs[0])
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || len(asst["tool_calls"].([]any)) != 2 {
		t.Fatalf("assistant tool_calls merge failed: %v", asst)
	}
	if msgs[3].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool msg: %v", msgs[3])
	}
	if msgs[4].(map[string]any)["content"] != "12:00" {
		t.Fatalf("tool output parts join failed: %v", msgs[4])
	}

	tools := out["tools"].([]any)
	if len(tools) != 1 { // web_search 被忽略
		t.Fatalf("tools=%v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool shape: %v", tools[0])
	}
	if out["reasoning_effort"] != "high" || out["max_tokens"].(float64) != 2048 {
		t.Fatalf("reasoning/max_tokens: %v", out)
	}
	if out["stream"] != true {
		t.Fatal("stream not forwarded")
	}
	if ec.effort != "high" || ec.toolChoice != "auto" {
		t.Fatalf("echo: %+v", ec)
	}
}

func TestConvertRejectsStateful(t *testing.T) {
	var req request
	mustJSON(t, `{"model":"m","input":"hi","previous_response_id":"resp_123"}`, &req)
	if _, _, err := convertRequest(&req); err == nil {
		t.Fatal("expected rejection")
	}

	mustJSON(t, `{"model":"m","input":[{"type":"item_reference","id":"fc_1"}]}`, &req)
	if _, _, err := convertRequest(&req); err == nil {
		t.Fatal("expected item_reference rejection")
	}
}

func TestConvertDeveloperRole(t *testing.T) {
	var req request
	mustJSON(t, `{"model":"m","input":[{"type":"message","role":"developer","content":"规则"}]}`, &req)
	body, _, err := convertRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	mustJSON(t, string(body), &out)
	if out["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer not normalized: %v", out["messages"])
	}
}

// ---------- 非流式响应转换 ----------

func TestCompletionToResponse(t *testing.T) {
	completion := map[string]any{
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "晴天",
				"reasoning_content": "想一下",
			},
		}},
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0, "total_tokens": 15.0},
	}
	resp := completionToResponse(completion, &echo{model: "glm-5.2", tools: []tool{}, toolChoice: "auto"})

	if resp["status"] != "completed" || resp["object"] != "response" {
		t.Fatalf("resp=%v", resp)
	}
	output := resp["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" || output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output=%v", output)
	}
	u := resp["usage"].(*usage)
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestCompletionToResponseToolCallAndLength(t *testing.T) {
	completion := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "length",
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "f", "arguments": "{\"a\":1}"},
				}},
			},
		}},
	}
	resp := completionToResponse(completion, &echo{model: "m", tools: []tool{}, toolChoice: "auto"})
	if resp["status"] != "incomplete" {
		t.Fatalf("status=%v", resp["status"])
	}
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details=%v", resp["incomplete_details"])
	}
	output := resp["output"].([]any)
	// 无 content：只有 function_call，不应有占位 message
	if len(output) != 1 || output[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("output=%v", output)
	}
	if output[0].(map[string]any)["call_id"] != "call_1" {
		t.Fatalf("fc=%v", output[0])
	}
}

func TestCompletionToResponseEmptyContentPlaceholder(t *testing.T) {
	completion := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": ""},
		}},
	}
	resp := completionToResponse(completion, &echo{model: "m", tools: []tool{}, toolChoice: "auto"})
	output := resp["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("output=%v", output)
	}
	text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != " " {
		t.Fatalf("placeholder=%q", text)
	}
}

// ---------- 端到端：流式 ----------

// fakeChat 模拟 chat 面：返回一段含推理 + 正文 + 工具调用的 SSE。
func fakeChat(t *testing.T, sse string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("rewritten body not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	})
}

func collectEvents(t *testing.T, resp *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, block := range strings.Split(resp.Body.String(), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				dataLine = strings.TrimSpace(line[len("data:"):])
			}
		}
		if dataLine == "" {
			t.Fatalf("block without data: %q", block)
		}
		var ev map[string]any
		mustJSON(t, dataLine, &ev)
		events = append(events, ev)
	}
	return events
}

func eventTypes(events []map[string]any) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e["type"].(string)
	}
	return out
}

func TestStreamEndToEnd(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想"}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"一下"}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"晴"}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"天"}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"bj\"}"}}]}}]}`,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	h := Wrap(fakeChat(t, sse))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"天气","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("code=%d ct=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	events := collectEvents(t, rec)
	types := eventTypes(events)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		// 首个正文 delta 到达 → 先关 reasoning 再开 message
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		// 首个 tool_call 到达 → 先关 message 再开 function_call
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", types, want)
	}

	// 终帧校验
	final := events[len(events)-1]["response"].(map[string]any)
	if final["status"] != "completed" {
		t.Fatalf("final status=%v", final["status"])
	}
	output := final["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output len=%d", len(output))
	}
	fc := output[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["arguments"] != `{"city":"bj"}` {
		t.Fatalf("fc=%v", fc)
	}
	u := final["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 8 {
		t.Fatalf("usage=%v", u)
	}
	// 序号单调递增
	for i, e := range events {
		if e["sequence_number"].(float64) != float64(i+1) {
			t.Fatalf("seq at %d: %v", i, e["sequence_number"])
		}
	}
}

func TestStreamMidStreamError(t *testing.T) {
	sse := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n" +
		`data: {"error":{"message":"boom"}}` + "\n\n"
	h := Wrap(fakeChat(t, sse))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	events := collectEvents(t, rec)
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last=%v", last["type"])
	}
	respObj := last["response"].(map[string]any)
	if respObj["error"].(map[string]any)["message"] != "boom" {
		t.Fatalf("err=%v", respObj["error"])
	}
}

func TestStreamThinkingOnlyGetsPlaceholder(t *testing.T) {
	sse := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	h := Wrap(fakeChat(t, sse))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	events := collectEvents(t, rec)
	final := events[len(events)-1]["response"].(map[string]any)
	output := final["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" ||
		output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output=%v", output)
	}
	text := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != " " {
		t.Fatalf("placeholder=%q", text)
	}
}

// ---------- 端到端：非流式 ----------

func TestNonStreamEndToEnd(t *testing.T) {
	chat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","model":"glm-5.2","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"你好","reasoning_content":"嗯"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	})
	h := Wrap(chat)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	mustJSON(t, rec.Body.String(), &resp)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("resp=%v", resp)
	}
	output := resp["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output=%v", output)
	}
}

// ---------- 错误透传与校验 ----------

func TestErrorPassthrough(t *testing.T) {
	chat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
	})
	h := Wrap(chat)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "rate limited") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBadRequests(t *testing.T) {
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("chat handler should not be reached")
	}))
	for _, body := range []string{
		`{"input":"hi"}`,                          // 缺 model
		`{"model":"m"}`,                           // 缺 input
		`{"model":"m","input":"hi","previous_response_id":"r1"}`,
		`not json`,
	} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("body=%s code=%d", body, rec.Code)
		}
	}
}

// fakeChatImplicitHeader 模拟 upstream.StreamHint 的真实形态：
// 只 w.Header().Set(...)，从不显式 WriteHeader，首个 Write 隐式 200。
func fakeChatImplicitHeader(sse string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, sse) // 隐式 WriteHeader(200)
	})
}

func TestStreamImplicitHeader(t *testing.T) {
	sse := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	h := Wrap(fakeChatImplicitHeader(sse))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("code=%d ct=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	events := collectEvents(t, rec)
	if events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("events=%v", eventTypes(events))
	}
}

// ---------- 工具 ----------

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("json: %v\n%s", err, s)
	}
}
