// Package responses 提供 OpenAI Responses API（POST /v1/responses）兼容层。
//
// 设计：本包是一个「协议换壳」——把 Responses 方言请求转成 Chat Completions
// 请求，转发给既有的 chat 处理器（含鉴权 / 账号池 / 粘性 / 错误策略全套治理），
// 再把返回的 chat SSE 流或聚合 JSON 转回 Responses 事件流 / response 对象。
// 因此本包不 import pool/upstream 等任何治理逻辑，只依赖「下一个 handler」。
//
// 移植蓝本：tencent-openai-proxy（TypeScript）的 openai-responses.ts +
// converters/request/responses-to-openai-chat.ts + stream/responses-event-stream.ts，
// 按本网关的现实裁剪：
//   - 无状态：previous_response_id / item_reference 直接 400（上游无会话存储）；
//   - 推理经 reasoning_content / reasoning 字段流出（网关侧已归一），
//     不做 <think> 标签拆分（本网关的出站净化已消除标签形态）；
//   - 仅支持扁平 function 工具；web_search 等内置工具忽略并告警。
package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Wrap 把 chat handler 包成同时支持 /v1/responses 的根 handler：
// 该路径走 Responses 转换，其余全部透传给 chat handler（含 /v1/chat/completions、
// /v1/models、/status、/healthz、/panel/）。鉴权由 chat handler 侧 withAuth 完成——
// 本包原样转发 Authorization 头，不另起炉灶。
func Wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		serveResponses(w, r, next)
	})
	mux.Handle("/", next)
	return mux
}

// ---------- 请求侧：Responses → Chat Completions ----------

// request 是 Responses API 请求体中本网关关心的字段；未知字段一律忽略。
type request struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"` // string 或 []inputItem
	Instructions       string          `json:"instructions"`
	Tools              []tool          `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Reasoning          *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	MaxOutputTokens    *int     `json:"max_output_tokens"`
	Temperature        *float64 `json:"temperature"`
	TopP               *float64 `json:"top_p"`
	Stream             bool     `json:"stream"`
	PreviousResponseID string   `json:"previous_response_id"`
}

// inputItem 覆盖 message / function_call / function_call_output / reasoning /
// item_reference 五种形态的并集字段；按 type 分派，缺省 type 视为 message。
type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []contentPart
	ID      string          `json:"id"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// function_call_output
	Output json.RawMessage `json:"output"` // string 或 [{type,text}]
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"` // input_image
}

type tool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict"`
}

// badRequest 携带 HTTP 语义的转换错误。
type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// echo 是每个 response 对象都要回显的请求字段集合。
type echo struct {
	model          string
	instructions   any // string 或 nil
	tools          []tool
	toolChoice     any
	effort         any
	temperature    *float64
	topP           *float64
	maxOutputToks  *int
}

// convertRequest 把 Responses 请求转换为 Chat Completions 请求体（JSON bytes）。
func convertRequest(req *request) ([]byte, *echo, error) {
	if req.Model == "" {
		return nil, nil, &badRequest{`Field "model" is required`}
	}
	if len(req.Input) == 0 {
		return nil, nil, &badRequest{`Field "input" is required`}
	}
	if req.PreviousResponseID != "" {
		return nil, nil, &badRequest{
			"previous_response_id is not supported: this gateway is stateless. " +
				"Send the full conversation in `input` instead."}
	}

	messages := make([]map[string]any, 0, 8)

	if req.Instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.Instructions})
	}

	// input：字符串或 item 数组（用首非空字符判形态，避免二次解析）。
	var items []inputItem
	trimmed := strings.TrimSpace(string(req.Input))
	switch {
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(req.Input, &s); err != nil {
			return nil, nil, &badRequest{"invalid input: " + err.Error()}
		}
		messages = append(messages, map[string]any{"role": "user", "content": s})
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, nil, &badRequest{"invalid input: " + err.Error()}
		}
		for i := range items {
			if err := convertInputItem(&items[i], &messages); err != nil {
				return nil, nil, err
			}
		}
	default:
		return nil, nil, &badRequest{"input must be a string or an array of items"}
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}

	// 工具：扁平 function → Chat 嵌套形态；其余类型忽略（上游不支持）。
	chatTools := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.Type != "function" || t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Parameters != nil {
			fn["parameters"] = t.Parameters
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
	}
	if len(chatTools) > 0 {
		out["tools"] = chatTools
		if tc := convertToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}

	ec := &echo{
		model:         req.Model,
		tools:         req.Tools,
		toolChoice:    toolChoiceEcho(req.ToolChoice),
		temperature:   req.Temperature,
		topP:          req.TopP,
		maxOutputToks: req.MaxOutputTokens,
	}
	if req.Instructions != "" {
		ec.instructions = req.Instructions
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		ec.effort = req.Reasoning.Effort
	}
	if ec.tools == nil {
		ec.tools = []tool{}
	}
	return body, ec, nil
}

// convertInputItem 把一个 input item 转成 Chat 消息并追加到 messages。
func convertInputItem(item *inputItem, messages *[]map[string]any) error {
	typ := item.Type
	if typ == "" {
		typ = "message"
	}
	switch typ {
	case "message":
		return convertMessageItem(item, messages)
	case "function_call":
		// 连续的 function_call 合并进同一条 assistant 消息（Chat 的并行工具调用形态）。
		entry := map[string]any{
			"id":   item.CallID,
			"type": "function",
			"function": map[string]any{"name": item.Name, "arguments": item.Arguments},
		}
		if n := len(*messages); n > 0 {
			if last := (*messages)[n-1]; last["role"] == "assistant" {
				if tcs, ok := last["tool_calls"].([]map[string]any); ok {
					last["tool_calls"] = append(tcs, entry)
					return nil
				}
			}
		}
		*messages = append(*messages, map[string]any{
			"role": "assistant", "content": nil,
			"tool_calls": []map[string]any{entry},
		})
		return nil
	case "function_call_output":
		text, err := stringifyToolOutput(item.Output)
		if err != nil {
			return err
		}
		*messages = append(*messages, map[string]any{
			"role": "tool", "tool_call_id": item.CallID, "content": text,
		})
		return nil
	case "reasoning":
		// 推理历史无 Chat 等价物，回放 summary 只是噪声——跳过。
		return nil
	case "item_reference":
		return &badRequest{fmt.Sprintf(
			"item_reference %q cannot be resolved: this gateway does not store responses.", item.ID)}
	default:
		return &badRequest{fmt.Sprintf("Unsupported input item type: %q", typ)}
	}
}

func convertMessageItem(item *inputItem, messages *[]map[string]any) error {
	role := item.Role
	if role == "developer" {
		role = "system"
	}
	if role == "" {
		role = "user"
	}

	trimmed := strings.TrimSpace(string(item.Content))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(item.Content, &s); err != nil {
			return &badRequest{"invalid message content: " + err.Error()}
		}
		*messages = append(*messages, map[string]any{"role": role, "content": s})
		return nil
	}

	var parts []contentPart
	if err := json.Unmarshal(item.Content, &parts); err != nil {
		return &badRequest{"invalid message content: " + err.Error()}
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "refusal", "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			out = append(out, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": p.ImageURL},
			})
		default:
			return &badRequest{fmt.Sprintf("Unsupported content part type in message: %q", p.Type)}
		}
	}
	if role == "system" {
		// system 只携文本（多模态 system 无意义，Chat 侧也不需要 parts 形态）。
		var sb strings.Builder
		for _, p := range out {
			if s, ok := p["text"].(string); ok {
				sb.WriteString(s)
			}
		}
		*messages = append(*messages, map[string]any{"role": "system", "content": sb.String()})
		return nil
	}
	*messages = append(*messages, map[string]any{"role": role, "content": out})
	return nil
}

// stringifyToolOutput：工具结果是 string 或 [{type,text}]，仅文本能过 Chat 边界。
func stringifyToolOutput(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", &badRequest{"invalid function_call_output.output: " + err.Error()}
		}
		return s, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", &badRequest{"invalid function_call_output.output: " + err.Error()}
	}
	var sb strings.Builder
	for i, p := range parts {
		if p.Text == "" {
			continue
		}
		if sb.Len() > 0 || i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// convertToolChoice：auto/none/required 直传；{type:function,name} → Chat 形态；
// 其余（allowed_tools/mcp/…）上游无法兑现，回落 auto（即不发）。
func convertToolChoice(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "auto", "none", "required":
				return s
			}
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "function" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// toolChoiceEcho：response 对象上的回显（原样回传，不兑现语义）。
func toolChoiceEcho(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "auto"
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return "auto"
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return "auto"
}
