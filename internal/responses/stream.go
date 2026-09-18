package responses

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// streamConverter 消费网关 chat 面的 SSE 流（data: chat.completion.chunk），
// 实时改写为 Responses 事件序列：
//
//	response.created → response.in_progress →
//	[response.output_item.added → …delta… → response.output_item.done] × N →
//	response.completed（或 .incomplete / .failed）
//
// 输出项按 Responses 客户端期望的顺序惰性开启：reasoning → message → function_call。
// 移植自 proxy 的 stream/responses-event-stream.ts，裁剪掉 <think> 标签拆分
// （本网关的推理统一走 reasoning_content / reasoning 字段）。
type streamConverter struct {
	w  http.ResponseWriter
	fl http.Flusher
	ec *echo

	sequence  int
	respID    string
	createdAt int64
	model     string

	nextOutputIndex int
	openReasoning   *openItem
	openMessage     *openItem
	toolCalls       map[int]*toolCallState
	toolOrder       []int // Go map 迭代无序，用切片保 function_call 的开启顺序

	emitted []any
	sawText bool

	u          usage
	hasUsage   bool
	status     string // completed / incomplete
	failed     bool
	lineBuf    []byte
}

type openItem struct {
	itemID      string
	outputIndex int
	text        bytes.Buffer
}

type toolCallState struct {
	itemID      string
	callID      string
	name        string
	args        bytes.Buffer
	outputIndex int
}

func newStreamConverter(w http.ResponseWriter, ec *echo) *streamConverter {
	fl, _ := w.(http.Flusher)
	return &streamConverter{
		w:         w,
		fl:        fl,
		ec:        ec,
		respID:    newID("resp"),
		createdAt: time.Now().Unix(),
		model:     ec.model,
		toolCalls: map[int]*toolCallState{},
		status:    "completed",
	}
}

// start 发送 response.created / response.in_progress。
func (s *streamConverter) start() {
	resp := buildEnvelope(s.ec, s.model, nil, nil, "in_progress", nil)
	resp["id"] = s.respID
	resp["created_at"] = s.createdAt
	s.send("response.created", map[string]any{"type": "response.created", "response": resp})
	s.send("response.in_progress", map[string]any{"type": "response.in_progress", "response": resp})
}

// feed 喂入原始 SSE 字节；可能含不完整行，内部缓冲。仅处理完整帧。
func (s *streamConverter) feed(p []byte) {
	s.lineBuf = append(s.lineBuf, p...)
	for {
		i := bytes.IndexByte(s.lineBuf, '\n')
		if i < 0 {
			return
		}
		line := s.lineBuf[:i]
		s.lineBuf = s.lineBuf[i+1:]
		s.handleLine(string(line))
	}
}

func (s *streamConverter) handleLine(line string) {
	if s.failed {
		return
	}
	if len(line) == 0 || line == "\r" {
		return
	}
	if !bytes.HasPrefix([]byte(line), []byte("data:")) {
		return // event: / 注释 / 心跳一律忽略
	}
	data := bytes.TrimSpace([]byte(line[len("data:"):]))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return // 残帧直接丢弃：协议转换层不做 JSON 修复（上游帧由网关白名单重建，结构可信）
	}
	s.processChunk(chunk)
}

func (s *streamConverter) processChunk(chunk map[string]any) {
	if s.failed {
		return
	}

	// 网关透传的流中 error 帧 → response.failed 终止序列。
	if e, ok := chunk["error"].(map[string]any); ok && e != nil {
		msg, _ := e["message"].(string)
		if msg == "" {
			msg = "upstream stream error"
		}
		s.fail(msg)
		return
	}

	if m, ok := chunk["model"].(string); ok && m != "" {
		s.model = m
	}

	if u := usageFromChat(chunk["usage"]); u != nil {
		s.u = *u
		s.hasUsage = true
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	// 推理增量：三种已知拼写。
	if v, ok := delta["reasoning_content"].(string); ok && v != "" {
		s.onThinking(v)
	} else if v, ok := delta["reasoning"].(string); ok && v != "" {
		s.onThinking(v)
	}
	if t, ok := delta["thinking"].(map[string]any); ok {
		if v, ok := t["content"].(string); ok && v != "" {
			s.onThinking(v)
		}
	}

	if v, ok := delta["content"].(string); ok && v != "" {
		s.onText(v)
	}

	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		s.onToolCalls(tcs)
	}

	if fr, ok := choice["finish_reason"].(string); ok && fr != "" && fr != "null" {
		if fr == "length" {
			s.status = "incomplete"
		}
	}
}

// ---------- 输出项生命周期 ----------

func (s *streamConverter) onThinking(text string) {
	// 回答开始后的迟到推理丢弃（与 proxy 的 Anthropic/Responses 面行为一致：
	// 交错推理会产生混乱的输出项序列）。
	if s.failed || s.openMessage != nil || text == "" {
		return
	}
	if s.openReasoning == nil {
		itemID := newID("rs")
		idx := s.nextOutputIndex
		s.nextOutputIndex++
		s.send("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]any{"id": itemID, "type": "reasoning", "summary": []any{}, "status": "in_progress"},
		})
		s.send("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "item_id": itemID,
			"output_index": idx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
		s.openReasoning = &openItem{itemID: itemID, outputIndex: idx}
	}
	s.openReasoning.text.WriteString(text)
	s.send("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": s.openReasoning.itemID,
		"output_index": s.openReasoning.outputIndex, "summary_index": 0, "delta": text,
	})
}

func (s *streamConverter) onText(text string) {
	if s.failed || text == "" {
		return
	}
	s.closeReasoning()
	s.sawText = true
	if s.openMessage == nil {
		itemID := newID("msg")
		idx := s.nextOutputIndex
		s.nextOutputIndex++
		s.send("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]any{
				"id": itemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		s.send("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": itemID,
			"output_index": idx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		s.openMessage = &openItem{itemID: itemID, outputIndex: idx}
	}
	s.openMessage.text.WriteString(text)
	s.send("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": s.openMessage.itemID,
		"output_index": s.openMessage.outputIndex, "content_index": 0, "delta": text,
	})
}

func (s *streamConverter) onToolCalls(tcs []any) {
	seen := map[int]bool{}
	for _, raw := range tcs {
		if s.failed {
			return
		}
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if f, ok := tc["index"].(float64); ok {
			idx = int(f)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true

		st := s.toolCalls[idx]
		if st == nil {
			// 新工具调用关闭已开项：Responses 的输出项是顺序排列的。
			s.closeMessage()
			s.closeReasoning()
			itemID := newID("fc")
			outIdx := s.nextOutputIndex
			s.nextOutputIndex++
			callID, _ := tc["id"].(string)
			if callID == "" {
				callID = newID("call")
			}
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			s.send("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": outIdx,
				"item": map[string]any{
					"id": itemID, "type": "function_call", "call_id": callID,
					"name": name, "arguments": "", "status": "in_progress",
				},
			})
			st = &toolCallState{itemID: itemID, callID: callID, name: name, outputIndex: outIdx}
			s.toolCalls[idx] = st
			s.toolOrder = append(s.toolOrder, idx)
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if frag, ok := fn["arguments"].(string); ok && frag != "" {
				st.args.WriteString(frag)
				s.send("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": st.itemID,
					"output_index": st.outputIndex, "delta": frag,
				})
			}
		}
	}
}

// ---------- 关闭输出项（同时记录进最终 envelope） ----------

func (s *streamConverter) closeReasoning() {
	o := s.openReasoning
	if o == nil {
		return
	}
	s.openReasoning = nil
	text := o.text.String()
	item := reasoningItem(o.itemID, text)
	s.send("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": o.itemID,
		"output_index": o.outputIndex, "summary_index": 0, "text": text,
	})
	s.send("response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": o.itemID,
		"output_index": o.outputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	})
	s.send("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": o.outputIndex, "item": item,
	})
	s.emitted = append(s.emitted, item)
}

func (s *streamConverter) closeMessage() {
	o := s.openMessage
	if o == nil {
		return
	}
	s.openMessage = nil
	text := o.text.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	item := messageItem(o.itemID, text)
	s.send("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": o.itemID,
		"output_index": o.outputIndex, "content_index": 0, "text": text,
	})
	s.send("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": o.itemID,
		"output_index": o.outputIndex, "content_index": 0, "part": part,
	})
	s.send("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": o.outputIndex, "item": item,
	})
	s.emitted = append(s.emitted, item)
}

func (s *streamConverter) closeToolCalls() {
	for _, idx := range s.toolOrder {
		st := s.toolCalls[idx]
		args := st.args.String()
		if args == "" {
			args = "{}"
		}
		item := functionCallItem(st.itemID, st.callID, st.name, args)
		s.send("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": st.itemID,
			"output_index": st.outputIndex, "arguments": args,
		})
		s.send("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.outputIndex, "item": item,
		})
		s.emitted = append(s.emitted, item)
	}
	s.toolCalls = map[int]*toolCallState{}
	s.toolOrder = nil
}

// finish 收尾：关闭所有打开项、usage 兜底、发 response.completed/.incomplete。
// 幂等；failed 后为 no-op。
func (s *streamConverter) finish() {
	if s.failed {
		return
	}
	// 行缓冲里可能还有一帧无尾换行的残帧。
	if len(bytes.TrimSpace(s.lineBuf)) > 0 {
		s.handleLine(string(s.lineBuf))
		s.lineBuf = nil
	}

	s.closeMessage()
	s.closeToolCalls()
	s.closeReasoning()

	// 零文本零工具调用（含纯思考）：补单空格占位消息，理由同 completionToResponse。
	if !s.sawText {
		allReasoning := true
		for _, it := range s.emitted {
			if m, ok := it.(map[string]any); ok && m["type"] != "reasoning" {
				allReasoning = false
				break
			}
		}
		if allReasoning {
			s.onText(" ")
			s.closeMessage()
		}
	}

	// usage 兜底：宁高勿低。
	if !s.hasUsage || (s.u.InputTokens == 0 && s.u.OutputTokens == 0) {
		out := 0
		for _, it := range s.emitted {
			m, _ := it.(map[string]any)
			switch m["type"] {
			case "message":
				if c, ok := m["content"].([]any); ok {
					for _, p := range c {
						if pm, ok := p.(map[string]any); ok {
							if t, ok := pm["text"].(string); ok {
								out += estimateTokens(t)
							}
						}
					}
				}
			case "reasoning":
				if c, ok := m["summary"].([]any); ok {
					for _, p := range c {
						if pm, ok := p.(map[string]any); ok {
							if t, ok := pm["text"].(string); ok {
								out += estimateTokens(t)
							}
						}
					}
				}
			case "function_call":
				if a, ok := m["arguments"].(string); ok {
					out += estimateTokens(a)
				}
			}
		}
		if s.u.OutputTokens == 0 {
			s.u.OutputTokens = out
		}
		s.u.TotalTokens = s.u.InputTokens + s.u.OutputTokens
	}

	event := "response.completed"
	if s.status == "incomplete" {
		event = "response.incomplete"
	}
	resp := buildEnvelope(s.ec, s.model, s.emitted, &s.u, s.status, nil)
	resp["id"] = s.respID
	resp["created_at"] = s.createdAt
	s.send(event, map[string]any{"type": event, "response": resp})
	s.failed = true // 复用该标记阻止 finish 之后的任何事件
}

// fail 发送 response.failed 并终止序列。
func (s *streamConverter) fail(message string) {
	if s.failed {
		return
	}
	s.failed = true
	resp := buildEnvelope(s.ec, s.model, nil, nil, "failed",
		map[string]any{"code": "upstream_error", "message": message})
	resp["id"] = s.respID
	resp["created_at"] = s.createdAt
	s.send("response.failed", map[string]any{"type": "response.failed", "response": resp})
}

func (s *streamConverter) send(event string, data map[string]any) {
	s.sequence++
	data["sequence_number"] = s.sequence
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	s.w.Write([]byte("event: " + event + "\n"))
	s.w.Write([]byte("data: "))
	s.w.Write(payload)
	s.w.Write([]byte("\n\n"))
	if s.fl != nil {
		s.fl.Flush()
	}
}
