package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ── Anthropic /v1/messages ──────────────────────────

type anthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []anthropicMessage   `json:"messages"`
	System        json.RawMessage      `json:"system"`
	MaxTokens     *int                 `json:"max_tokens"`
	Stream        bool                 `json:"stream"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	StopSequences []string             `json:"stop_sequences"`
	Tools         []anthropicTool      `json:"tools"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice"`
	Thinking      *anthropicThinking   `json:"thinking"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Thinking     string          `json:"thinking"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens"`
	Effort       string `json:"effort"`
}

func (s *proxyServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	const path = "/v1/messages"
	start := time.Now()

	body, release, err := readBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeAnthropicError(w, 413, "invalid_request_error", s.bodyLimitMessage(), 0)
			return
		}
		writeAnthropicError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		release()
		writeAnthropicError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	release()

	ar, err := s.authorize(r)
	if err != nil {
		e := authFailure(err)
		writeAnthropicError(w, e.Status, e.Type, e.Message, 0)
		return
	}

	chatReq := convertAnthropicToChat(&req)
	model := req.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	ccBody, err := buildCCBody(s.cfg, chatReq)
	if err != nil {
		writeAnthropicError(w, 500, "internal_error", "Failed to build upstream request", 0)
		return
	}

	sessionID := sessionFromHeaders(r, chatReq.PromptCacheKey)
	if sessionID == "" {
		sessionID = ar.upstream.sessionIDFor(time.Now())
	}

	resp, usedKey, err := s.generateWithFailover(r.Context(), ar, ccBody, sessionID,
		s.cc.projectSlugFor(sessionID), req.Stream, wantsZDR(r, s.cfg.ZDR))
	if err != nil {
		if upstreamCancelled(err) {
			s.log.Warn("Request cancelled before CC response", "path", path, "model", model, "elapsedMs", sinceMs(start))
			return
		}
		s.log.Error("Upstream error", "message", err.Error(), "path", path)
		writeAnthropicError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
		return
	}
	if resp.StatusCode/100 != 2 {
		mapped := mapCCStatus(resp.StatusCode, readErrorBody(resp))
		s.noteUpstreamModelRejection(model, mapped.Message)
		s.log.Error("CC API error (Anthropic)", "status", resp.StatusCode, "key", usedKey.label())
		writeAnthropicError(w, mapped.Status, mapped.Type, mapped.Message, mapped.RetryAfter)
		return
	}
	defer drainAndClose(resp.Body)

	messageID := "msg_" + randHex(6)
	if req.Stream {
		s.messagesStream(w, resp, model, messageID, start, path)
	} else {
		s.messagesNonStream(w, resp, model, messageID, start, path)
	}
}

// ── Anthropic → 内部 Chat 表示 ──────────────────────

func convertAnthropicToChat(req *anthropicRequest) *chatRequest {
	out := &chatRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}
	if req.MaxTokens != nil {
		out.MaxTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		out.Temperature = req.Temperature
	}
	if req.TopP != nil {
		out.TopP = req.TopP
	}
	if req.Metadata != nil {
		out.User = req.Metadata.UserID
	}

	if sys := anthropicSystemText(req.System); sys != "" {
		out.Messages = append(out.Messages, chatMessage{
			Role:    "system",
			Content: mustJSON(sys),
		})
	}

	toolNames := make(map[string]string, 8)
	for i := range req.Messages {
		msg := &req.Messages[i]
		blocks := anthropicBlocks(msg.Content)
		switch msg.Role {
		case "assistant":
			text := bytes.Buffer{}
			thinking := bytes.Buffer{}
			var toolCalls []chatToolCall
			for j := range blocks {
				b := &blocks[j]
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "thinking":
					thinking.WriteString(b.Thinking)
				case "tool_use":
					if b.ID != "" {
						toolNames[b.ID] = b.Name
					}
					tc := chatToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(rawOrEmpty(b.Input))
					if bytes.Equal(bytes.TrimSpace(b.Input), []byte("null")) || len(b.Input) == 0 {
						tc.Function.Arguments = "{}"
					}
					toolCalls = append(toolCalls, tc)
				}
			}
			m := chatMessage{Role: "assistant", ToolCalls: toolCalls}
			if text.Len() > 0 {
				m.Content = mustJSON(text.String())
			}
			if thinking.Len() > 0 {
				m.ReasoningContent = thinking.String()
			}
			out.Messages = append(out.Messages, m)
		case "user":
			text := bytes.Buffer{}
			var toolResults []anthropicBlock
			for j := range blocks {
				b := &blocks[j]
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_result":
					toolResults = append(toolResults, *b)
				}
			}
			// OpenAI 语义要求 tool 消息紧跟 assistant 的 tool_calls：
			// 同一条 user 消息里的 tool_result 先入队，文本排在后面。
			for i := range toolResults {
				tr := &toolResults[i]
				m := chatMessage{Role: "tool", ToolCallID: tr.ToolUseID}
				m.Content = mustJSON(toolResultText(tr.Content))
				if name := toolNames[tr.ToolUseID]; name != "" {
					m.Name = name
				}
				out.Messages = append(out.Messages, m)
			}
			if text.Len() > 0 {
				out.Messages = append(out.Messages, chatMessage{Role: "user", Content: mustJSON(text.String())})
			}
		}
	}

	if len(req.Tools) > 0 {
		tools := make([]chatTool, 0, len(req.Tools))
		for i := range req.Tools {
			t := &req.Tools[i]
			tool := chatTool{Type: "function"}
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool.Name = t.Name
			tool.Description = t.Description
			tool.Parameters = params
			tools = append(tools, tool)
		}
		out.Tools = tools
	}

	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "":
			out.ToolChoice = json.RawMessage(`"auto"`)
		case "any":
			out.ToolChoice = json.RawMessage(`"required"`)
		case "none":
			out.ToolChoice = json.RawMessage(`"none"`)
		case "tool":
			b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
			out.ToolChoice = b
		}
	}

	// Anthropic thinking → reasoning_effort（LiteLLM 标准映射）
	if t := req.Thinking; t != nil {
		switch {
		case t.Type == "disabled" || t.Type == "none":
			// 不发 reasoning_effort
		case t.Type == "adaptive":
			out.ReasoningEffort = orDefault(t.Effort, "medium")
		case t.BudgetTokens != nil:
			switch b := *t.BudgetTokens; {
			case b >= 10000:
				out.ReasoningEffort = "high"
			case b >= 5000:
				out.ReasoningEffort = "medium"
			default:
				out.ReasoningEffort = "low"
			}
		}
	}
	return out
}

func anthropicSystemText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
		return ""
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	var sb bytes.Buffer
	for i := range blocks {
		if blocks[i].Type != "text" && blocks[i].Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(blocks[i].Text)
	}
	return sb.String()
}

func anthropicBlocks(raw json.RawMessage) []anthropicBlock {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return []anthropicBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil
	}
	return blocks
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ── 流式 Anthropic SSE ──────────────────────────────

type anthropicStream struct {
	nextBlockIndex int64
	curIndex       int64
	curType        string
	blockStarted   bool
	thinking       bytes.Buffer
	outputTokens   int64
	inputTokens    int64 // 上游原始总数（含缓存），仅用于减法兜底
	cachedInput    int64
	cacheWrite     int64
	noCacheTokens  int64 // -1 = 上游未提供，改用减法
	stopReason     string
	hasContent     bool
	lastEvent      string
	bytes          int64
	lastWrite      time.Time
	upstreamErr    *mappedError
}

func newAnthropicStream(now time.Time) *anthropicStream {
	return &anthropicStream{noCacheTokens: -1, lastWrite: now}
}

func (s *proxyServer) messagesStream(w http.ResponseWriter, resp *http.Response, model, messageID string, start time.Time, path string) {
	sw := newSSEWriter(w, s.cfg.ClientStall)
	st := newAnthropicStream(start)
	buf := getJSONBuf()
	defer putJSONBuf(buf)

	readErr := readCCLines(resp.Body, func(line []byte) error {
		st.bytes += int64(len(line)) + 1
		if len(line) == 0 || bytes.Equal(line, doneLiteral) {
			return nil
		}
		var ev ccEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
			return nil
		}
		st.lastEvent = ev.Type

		switch ev.Type {
		case "text-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.hasContent = true
			if !sw.Started() {
				if err := writeAnthropicMessageStart(sw, buf, messageID, model); err != nil {
					return err
				}
			}
			if err := st.startBlock(sw, buf, "text"); err != nil {
				return err
			}
			st.outputTokens++
			st.lastWrite = time.Now()
			return st.deltaText(sw, buf, text)

		case "reasoning-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.hasContent = true
			if !sw.Started() {
				if err := writeAnthropicMessageStart(sw, buf, messageID, model); err != nil {
					return err
				}
			}
			if err := st.startBlock(sw, buf, "thinking"); err != nil {
				return err
			}
			st.thinking.WriteString(text)
			st.lastWrite = time.Now()
			return st.deltaThinking(sw, buf, text)

		case "tool-call":
			st.hasContent = true
			if !sw.Started() {
				if err := writeAnthropicMessageStart(sw, buf, messageID, model); err != nil {
					return err
				}
			}
			if err := st.closeBlock(sw, buf); err != nil {
				return err
			}
			id := ev.ToolCallID
			if id == "" {
				id = "toolu_" + randHex(6)
			}
			idx := st.nextBlockIndex
			st.nextBlockIndex++
			buf.reset()
			beginAnthropicEvent(buf, "content_block_start")
			buf.raw(`{"type":"content_block_start","index":`).int(idx).
				raw(`,"content_block":{"type":"tool_use","id":`).str(id).
				raw(`,"name":`).str(ev.ToolName).raw(`,"input":{}}}`)
			endFrame(buf)
			if err := sw.Write(buf.bytes()); err != nil {
				return err
			}
			st.outputTokens += 20
			st.lastWrite = time.Now()
			return st.deltaToolInput(sw, buf, idx, string(toolArgsJSON(ev.Input)))

		case "finish-step", "finish":
			if ev.FinishReason != "" {
				st.stopReason = mapAnthropicStopReason(mapFinishReason(ev.FinishReason))
			}
			u := ev.TotalUsage
			if u == nil {
				u = ev.Usage
			}
			if u != nil {
				normalizeCCUsage(u)
				if u.OutputTokens > 0 {
					st.outputTokens = u.OutputTokens
				}
				st.inputTokens = u.InputTokens
				st.cachedInput = u.cacheRead()
				st.cacheWrite = u.cacheWrite()
				if u.InputTokenDetails != nil && u.InputTokenDetails.NoCacheTokens != nil {
					st.noCacheTokens = *u.InputTokenDetails.NoCacheTokens
				}
			}
			return nil

		case "error":
			mapped := mapCCEventError(&ev)
			st.upstreamErr = &mapped
			s.log.Warn("CC stream error (Anthropic)", "message", mapped.Message)
			if err := sw.Start(); err != nil {
				return err
			}
			buf.reset()
			beginAnthropicEvent(buf, "error")
			buf.raw(`{"type":"error","error":{"type":`).str(mapped.Type).
				raw(`,"message":`).str(mapped.Message).raw("}}")
			endFrame(buf)
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "text-start", "reasoning-start", "start", "start-step",
			"reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
			"tool-input-end", "tool-error", "text-end":
			// 静默事件：已开始的流用 Anthropic 标准 ping 事件保活
			if sw.Started() && time.Since(st.lastWrite) > 15*time.Second {
				st.lastWrite = time.Now()
				return sw.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
			}
			return nil

		default:
			s.log.Warn("Unknown CC event type", "type", ev.Type)
			return nil
		}
	})

	if err := st.closeBlock(sw, buf); err != nil {
		readErr = err
	}

	if readErr != nil && !errors.Is(readErr, errClientGone) {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			msg := s.timeoutMessage()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", true,
				"timeoutMs", s.cfg.StreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent),
				"outputTokens", st.outputTokens)
			if !sw.Started() {
				writeAnthropicError(w, 429, "rate_limit_error", msg, 5)
				return
			}
			buf.reset()
			beginAnthropicEvent(buf, "error")
			buf.raw(`{"type":"error","error":{"type":"rate_limit_error","message":`).str(msg).raw(`},"retry_after":5}`)
			endFrame(buf)
			_ = sw.Write(buf.bytes())
			sw.Flush()
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Anthropic stream error", "message", readErr.Error(), "path", path)
		if !sw.Started() {
			writeAnthropicError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
			return
		}
		buf.reset()
		beginAnthropicEvent(buf, "error")
		buf.raw(`{"type":"error","error":{"type":"internal_error","message":`).str(readErr.Error()).raw("}}")
		endFrame(buf)
		_ = sw.Write(buf.bytes())
		sw.Flush()
		return
	}

	if st.upstreamErr != nil {
		if !sw.Started() {
			e := *st.upstreamErr
			writeAnthropicError(w, e.Status, e.Type, e.Message, e.RetryAfter)
			return
		}
		// 已经下发过 error 事件（规范里 error 即终结）
		sw.Flush()
		return
	}

	if st.outputTokens == 0 && !st.hasContent {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", true,
			"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent))
		if !sw.Started() {
			writeAnthropicError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
			return
		}
		sw.Flush()
		return
	}

	s.noteSuccess()
	if !sw.Started() {
		if err := writeAnthropicMessageStart(sw, buf, messageID, model); err != nil {
			return
		}
	}
	buf.reset()
	beginAnthropicEvent(buf, "message_delta")
	reason := st.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	// 只计非缓存部分；否则下游把 input 与 cache_read 相加会得到约两倍（issue #25）
	inputTokens := st.noCacheTokens
	if inputTokens < 0 {
		inputTokens = st.inputTokens - st.cachedInput - st.cacheWrite
		if inputTokens < 0 {
			inputTokens = 0
		}
	}
	buf.raw(`{"type":"message_delta","delta":{"stop_reason":`).str(reason).raw(`,"stop_sequence":null},"usage":{"output_tokens":`).
		int(st.outputTokens).
		raw(`,"cache_read_input_tokens":`).int(st.cachedInput).
		raw(`,"cache_creation_input_tokens":`).int(st.cacheWrite).
		raw(`,"input_tokens":`).int(inputTokens).raw("}}")
	endFrame(buf)
	if err := sw.Write(buf.bytes()); err != nil {
		return
	}
	buf.reset()
	beginAnthropicEvent(buf, "message_stop")
	buf.raw(`{"type":"message_stop"}`)
	endFrame(buf)
	_ = sw.Write(buf.bytes())
	sw.Flush()
}

func writeAnthropicMessageStart(sw *sseWriter, buf *jsonBuf, messageID, model string) error {
	// 首帧前启动 SSE 响应（200 + 头 + 已暂存内容）；Start 幂等
	if err := sw.Start(); err != nil {
		return err
	}
	buf.reset()
	beginAnthropicEvent(buf, "message_start")
	buf.raw(`{"type":"message_start","message":{"id":`).str(messageID).
		raw(`,"type":"message","role":"assistant","content":[],"model":`).str(model).
		raw(`,"usage":{"input_tokens":0,"output_tokens":0}}}`)
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *anthropicStream) startBlock(sw *sseWriter, buf *jsonBuf, kind string) error {
	if st.blockStarted && st.curType == kind {
		return nil
	}
	if err := st.closeBlock(sw, buf); err != nil {
		return err
	}
	st.curIndex = st.nextBlockIndex
	st.nextBlockIndex++
	st.curType = kind
	st.blockStarted = true
	buf.reset()
	beginAnthropicEvent(buf, "content_block_start")
	buf.raw(`{"type":"content_block_start","index":`).int(st.curIndex).
		raw(`,"content_block":{"type":`).str(kind).raw(`,"`).raw(kind).raw(`":""}}`)
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *anthropicStream) closeBlock(sw *sseWriter, buf *jsonBuf) error {
	if !st.blockStarted {
		return nil
	}
	idx := st.curIndex
	kind := st.curType
	st.blockStarted = false
	st.curType = ""
	if kind == "thinking" && st.thinking.Len() > 0 {
		sig := fakeThinkingSignature(st.thinking.String())
		st.thinking.Reset()
		buf.reset()
		beginAnthropicEvent(buf, "content_block_delta")
		buf.raw(`{"type":"content_block_delta","index":`).int(idx).
			raw(`,"delta":{"type":"signature_delta","signature":`).str(sig).raw("}}")
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
	}
	buf.reset()
	beginAnthropicEvent(buf, "content_block_stop")
	buf.raw(`{"type":"content_block_stop","index":`).int(idx).byte('}')
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *anthropicStream) deltaText(sw *sseWriter, buf *jsonBuf, text string) error {
	buf.reset()
	beginAnthropicEvent(buf, "content_block_delta")
	buf.raw(`{"type":"content_block_delta","index":`).int(st.curIndex).
		raw(`,"delta":{"type":"text_delta","text":`).str(text).raw("}}")
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *anthropicStream) deltaThinking(sw *sseWriter, buf *jsonBuf, text string) error {
	buf.reset()
	beginAnthropicEvent(buf, "content_block_delta")
	buf.raw(`{"type":"content_block_delta","index":`).int(st.curIndex).
		raw(`,"delta":{"type":"thinking_delta","thinking":`).str(text).raw("}}")
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *anthropicStream) deltaToolInput(sw *sseWriter, buf *jsonBuf, idx int64, partial string) error {
	buf.reset()
	beginAnthropicEvent(buf, "content_block_delta")
	buf.raw(`{"type":"content_block_delta","index":`).int(idx).
		raw(`,"delta":{"type":"input_json_delta","partial_json":`).str(partial).raw("}}")
	endFrame(buf)
	if err := sw.Write(buf.bytes()); err != nil {
		return err
	}
	buf.reset()
	beginAnthropicEvent(buf, "content_block_stop")
	buf.raw(`{"type":"content_block_stop","index":`).int(idx).byte('}')
	endFrame(buf)
	return sw.Write(buf.bytes())
}

// ── 非流式 Anthropic JSON ───────────────────────────

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type anthropicToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type anthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []any          `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence any            `json:"stop_sequence"`
	Usage        anthropicUsage `json:"usage"`
}

func (s *proxyServer) messagesNonStream(w http.ResponseWriter, resp *http.Response, model, messageID string, start time.Time, path string) {
	var (
		fullText    bytes.Buffer
		thinking    bytes.Buffer
		toolCalls   []chatToolCall
		usage       *ccUsage
		upstreamErr *mappedError
		lastEvent   string
		bytesRecv   int64
	)
	finishReason := "stop"

	readErr := readCCLines(resp.Body, func(line []byte) error {
		bytesRecv += int64(len(line)) + 1
		if len(line) == 0 || bytes.Equal(line, doneLiteral) {
			return nil
		}
		var ev ccEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
			return nil
		}
		lastEvent = ev.Type
		switch ev.Type {
		case "text-delta":
			fullText.WriteString(ev.textOf())
		case "reasoning-delta":
			thinking.WriteString(ev.textOf())
		case "tool-call":
			id := ev.ToolCallID
			if id == "" {
				id = "call_" + randHex(4)
			}
			tc := chatToolCall{ID: id, Type: "function"}
			tc.Function.Name = ev.ToolName
			tc.Function.Arguments = string(toolArgsJSON(ev.Input))
			toolCalls = append(toolCalls, tc)
		case "finish":
			finishReason = mapFinishReason(ev.FinishReason)
			if ev.TotalUsage != nil {
				usage = ev.TotalUsage
			} else if ev.Usage != nil {
				usage = ev.Usage
			}
		case "error":
			mapped := mapCCEventError(&ev)
			upstreamErr = &mapped
			s.log.Warn("CC error (Anthropic non-stream)", "message", mapped.Message)
		case "reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
			"tool-input-end", "tool-error", "text-end":
			// 静默
		default:
			s.log.Warn("Unknown CC event type", "type", ev.Type)
		}
		return nil
	})

	if readErr != nil {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", false,
				"timeoutMs", s.cfg.NonstreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent), "partialLen", fullText.Len())
			writeAnthropicError(w, 429, "rate_limit_error", s.timeoutMessage(), 5)
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Upstream error", "message", readErr.Error(), "path", path)
		writeAnthropicError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
		return
	}
	if upstreamErr != nil {
		writeAnthropicError(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Message, upstreamErr.RetryAfter)
		return
	}
	if fullText.Len() == 0 && thinking.Len() == 0 && len(toolCalls) == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", false,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		writeAnthropicError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}

	content := make([]any, 0, 3)
	if t := thinking.String(); t != "" {
		content = append(content, anthropicThinkingBlock{Type: "thinking", Thinking: t, Signature: fakeThinkingSignature(t)})
	}
	if txt := fullText.String(); txt != "" {
		content = append(content, anthropicTextBlock{Type: "text", Text: txt})
	}
	for i := range toolCalls {
		tc := &toolCalls[i]
		args := bytes.TrimSpace([]byte(tc.Function.Arguments))
		if len(args) == 0 {
			args = []byte("{}")
		}
		content = append(content, anthropicToolUseBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}

	u := usage
	if u == nil {
		u = &ccUsage{}
	}
	normalizeCCUsage(u)
	estOut := u.OutputTokens
	if estOut == 0 {
		// 上游没回报 usage 时按内容长度估算，避免客户端展示/记账为 0
		estOut = int64((fullText.Len()+thinking.Len())/4 + len(toolCalls)*20)
		if estOut < 1 {
			estOut = 1
		}
	}

	out := anthropicResponse{
		ID:           messageID,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      content,
		StopReason:   mapAnthropicStopReason(finishReason),
		StopSequence: nil,
		Usage: anthropicUsage{
			// input_tokens 只计非缓存部分（Anthropic 语义），与 cache_* 相加才等于总输入
			InputTokens:              anthropicInputTokens(u, -1),
			OutputTokens:             estOut,
			CacheCreationInputTokens: u.cacheWrite(),
			CacheReadInputTokens:     u.cacheRead(),
		},
	}
	s.noteSuccess()
	writeJSON(w, 200, out, 0)
}
