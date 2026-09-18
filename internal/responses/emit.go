package responses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// ---------- response 对象装配（非流式出口 + 流式 finalize 共用） ----------

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// usage 是 Responses 的用量形态。
type usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	InputCachedTokens   int `json:"-"`
	OutputReasoningToks int `json:"-"`
}

func (u *usage) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"input_tokens":  u.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": u.InputCachedTokens},
		"output_tokens": u.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.OutputReasoningToks},
		"total_tokens":  u.TotalTokens,
	})
}

// usageFromChat 从 chat.completion 的 usage 字段映射；缺失返回 nil。
func usageFromChat(v any) *usage {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	num := func(k string) int {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	u := &usage{
		InputTokens:  num("prompt_tokens"),
		OutputTokens: num("completion_tokens"),
		TotalTokens:  num("total_tokens"),
	}
	if d, ok := m["prompt_tokens_details"].(map[string]any); ok {
		if f, ok := d["cached_tokens"].(float64); ok {
			u.InputCachedTokens = int(f)
		}
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		if f, ok := d["reasoning_tokens"].(float64); ok {
			u.OutputReasoningToks = int(f)
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

// estimateTokens：usage 缺失时的兜底估算。契约与 proxy 一致：宁可高估，不可低估
// （客户端上下文管理依赖这个数，低估才有害）。
func estimateTokens(text string) int {
	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}
	return (n + 1) / 2 // 中英混合文本约 0.5-0.7 token/字，取偏高侧
}

// buildEnvelope 组装 response 外壳。status: in_progress / completed / incomplete / failed。
func buildEnvelope(ec *echo, model string, output []any, u *usage, status string, errObj any) map[string]any {
	if output == nil {
		output = []any{}
	}
	var incomplete any
	if status == "incomplete" {
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	return map[string]any{
		"id":                  newID("resp"),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"error":               errObj,
		"incomplete_details":  incomplete,
		"instructions":        ec.instructions,
		"max_output_tokens":   intPtrOrNil(ec.maxOutputToks),
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"previous_response_id": nil,
		"reasoning":           map[string]any{"effort": ec.effort, "summary": nil},
		"store":               false,
		"temperature":         floatPtrOrNil(ec.temperature),
		"text":                map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":         ec.toolChoice,
		"tools":               toolsEcho(ec.tools),
		"top_p":               floatPtrOrNil(ec.topP),
		"truncation":          "disabled",
		"usage":               u,
		"user":                nil,
		"metadata":            map[string]any{},
	}
}

func intPtrOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func floatPtrOrNil(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func toolsEcho(tools []tool) any {
	if len(tools) == 0 {
		return []any{}
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m := map[string]any{"type": t.Type}
		if t.Type == "function" {
			m["name"] = t.Name
			m["description"] = t.Description
			if t.Parameters != nil {
				m["parameters"] = t.Parameters
			}
			if t.Strict != nil {
				m["strict"] = *t.Strict
			}
		}
		out = append(out, m)
	}
	return out
}

// reasoningItem / messageItem / functionCallItem 构造 output 项。
func reasoningItem(id, text string) map[string]any {
	return map[string]any{
		"id":   id,
		"type": "reasoning",
		"summary": []any{
			map[string]any{"type": "summary_text", "text": text},
		},
		"status": nil,
	}
}

func messageItem(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		},
	}
}

func functionCallItem(id, callID, name, args string) map[string]any {
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"id": id, "type": "function_call", "call_id": callID,
		"name": name, "arguments": args, "status": "completed",
	}
}

// extractReasoning：chat message 上推理文本的三种已知拼写。
func extractReasoning(message map[string]any) string {
	if message == nil {
		return ""
	}
	if t, ok := message["thinking"].(map[string]any); ok {
		if s, ok := t["content"].(string); ok && s != "" {
			return s
		}
	}
	for _, k := range []string{"reasoning", "reasoning_content"} {
		if s, ok := message[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// completionToResponse 把聚合后的 chat.completion（map 形态）转成 response 对象。
//
// 无文本且无工具调用的回答补一个单空格占位——与 proxy 的修复一致：
// 空 assistant 输出会破坏 agent 循环，且轮回填历史时是非法消息。
func completionToResponse(completion map[string]any, ec *echo) map[string]any {
	status := "completed"
	output := make([]any, 0, 4)

	var message map[string]any
	var finishReason string
	if choices, ok := completion["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			message, _ = c0["message"].(map[string]any)
			finishReason, _ = c0["finish_reason"].(string)
		}
	}
	if finishReason == "length" {
		status = "incomplete"
	}

	if r := extractReasoning(message); r != "" {
		output = append(output, reasoningItem(newID("rs"), r))
	}

	var content string
	if s, ok := message["content"].(string); ok {
		content = s
	}
	var toolCalls []any
	if tcs, ok := message["tool_calls"].([]any); ok {
		toolCalls = tcs
	}

	if content != "" || len(toolCalls) == 0 {
		if content == "" {
			content = " "
		}
		output = append(output, messageItem(newID("msg"), content))
	}
	for _, tc := range toolCalls {
		m, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		callID, _ := m["id"].(string)
		output = append(output, functionCallItem(newID("fc"), callID, name, args))
	}

	model := ec.model
	if s, ok := completion["model"].(string); ok && s != "" {
		model = s
	}
	return buildEnvelope(ec, model, output, usageFromChat(completion["usage"]), status, nil)
}
