package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ── OpenAI Responses API（/v1/responses） ────────────
//
// 供 Codex 等使用 Responses 协议的客户端接入。代理仍是无状态转换层：
// 把 input 翻译成内部 Chat 表示，复用同一条 CC 转发管线。
// previous_response_id / store 需要服务端保存会话，与无状态定位冲突，直接 400。

type responsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       json.RawMessage     `json:"instructions"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Stream             bool                `json:"stream"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Tools              []responsesTool     `json:"tools"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	Reasoning          *responsesReasoning `json:"reasoning"`
	PreviousResponseID string              `json:"previous_response_id"`
	Store              *bool               `json:"store"`
	Metadata           json.RawMessage     `json:"metadata"`
	Text               json.RawMessage     `json:"text"`
	Truncation         string              `json:"truncation"`
	User               string              `json:"user"`
}

type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Text      string          `json:"text"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

func (s *proxyServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	const path = "/v1/responses"
	start := time.Now()

	body, release, err := readBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeResponsesError(w, 413, "invalid_request_error", s.bodyLimitMessage(), 0)
			return
		}
		writeResponsesError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		release()
		writeResponsesError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	release()

	if req.PreviousResponseID != "" {
		writeResponsesError(w, 400, "invalid_request_error",
			"previous_response_id is not supported (this proxy is stateless); send the full input each turn", 0)
		return
	}

	ar, err := s.authorize(r)
	if err != nil {
		e := authFailure(err)
		writeResponsesError(w, e.Status, e.Type, e.Message, 0)
		return
	}

	chatReq := convertResponsesToChat(&req)
	if len(chatReq.Messages) == 0 {
		writeResponsesError(w, 400, "invalid_request_error", "input is required", 0)
		return
	}
	model := chatReq.Model
	if model == "" {
		model = defaultChatModel
	}

	ccBody, err := buildCCBody(s.cfg, chatReq)
	if err != nil {
		writeResponsesError(w, 500, "internal_error", "Failed to build upstream request", 0)
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
		writeResponsesError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
		return
	}
	if resp.StatusCode/100 != 2 {
		mapped := mapCCStatus(resp.StatusCode, readErrorBody(resp))
		s.noteUpstreamModelRejection(model, mapped.Message)
		s.log.Error("CC API error", "status", resp.StatusCode, "path", path, "key", usedKey.label())
		writeResponsesError(w, mapped.Status, mapped.Type, mapped.Message, mapped.RetryAfter)
		return
	}
	defer drainAndClose(resp.Body)

	responseID := newResponsesID("resp_")
	created := unixNow()
	if req.Stream {
		s.responsesStream(w, resp, &req, model, responseID, created, start, path)
	} else {
		s.responsesNonStream(w, resp, &req, model, responseID, created, start, path)
	}
}

func newResponsesID(prefix string) string {
	return prefix + randHex(12)
}

func responsesTextOf(content json.RawMessage) string {
	trimmed := bytes.TrimSpace(content)
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
	if trimmed[0] != '[' {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return ""
	}
	var sb bytes.Buffer
	for i := range parts {
		sb.WriteString(parts[i].Text)
	}
	return sb.String()
}

func (it *responsesItem) reasoningText() string {
	for _, raw := range []json.RawMessage{it.Summary, it.Content} {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) != 4 || !bytes.HasPrefix(trimmed, []byte("[{")) {
			// 也可能就是普通数组，统一走下面的解析
		}
		if len(trimmed) == 0 {
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &parts); err == nil && len(parts) > 0 {
			var sb bytes.Buffer
			for i := range parts {
				sb.WriteString(parts[i].Text)
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}
	return it.Text
}

func convertResponsesToChat(req *responsesRequest) *chatRequest {
	out := &chatRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}
	if req.MaxOutputTokens != nil {
		out.MaxTokens = req.MaxOutputTokens
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	out.ParallelToolCalls = req.ParallelToolCalls
	out.User = req.User
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.ReasoningEffort = req.Reasoning.Effort
	}

	if sys := responsesTextOf(req.Instructions); sys != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: mustJSON(sys)})
	}

	// Responses 把 reasoning / message / function_call 拆成并列 item，
	// Chat 要求它们挂在同一条 assistant 消息上，故先累积再冲刷。
	var pending *chatMessage
	ensurePending := func() *chatMessage {
		if pending == nil {
			pending = &chatMessage{Role: "assistant"}
		}
		return pending
	}
	flushPending := func() {
		if pending == nil {
			return
		}
		if pending.Content == nil && len(pending.ToolCalls) == 0 && pending.ReasoningContent == "" {
			pending = nil
			return
		}
		out.Messages = append(out.Messages, *pending)
		pending = nil
	}

	input := bytes.TrimSpace(req.Input)
	switch {
	case len(input) == 0 || bytes.Equal(input, []byte("null")):
		// 没有 input
	case input[0] == '"':
		var s string
		if err := json.Unmarshal(input, &s); err == nil {
			out.Messages = append(out.Messages, chatMessage{Role: "user", Content: mustJSON(s)})
		}
	case input[0] == '[':
		var items []responsesItem
		if err := json.Unmarshal(input, &items); err == nil {
			for i := range items {
				item := &items[i]
				// 标准 Responses 数组项形如 {"role":"user","content":...}，无 type 字段；
				// 仅带 role 的 item 按消息处理，否则整条用户输入会被静默丢弃。
				itemType := item.Type
				if itemType == "" && item.Role != "" {
					itemType = "message"
				}
				switch itemType {
				case "reasoning":
					if t := item.reasoningText(); t != "" {
						ensurePending().ReasoningContent = t
					}
				case "message":
					text := responsesTextOf(item.Content)
					switch item.Role {
					case "assistant":
						if text != "" {
							ensurePending().Content = mustJSON(text)
						}
					case "system", "developer":
						flushPending()
						out.Messages = append(out.Messages, chatMessage{Role: "system", Content: mustJSON(text)})
					default:
						flushPending()
						out.Messages = append(out.Messages, chatMessage{Role: "user", Content: mustJSON(text)})
					}
				case "function_call":
					tc := chatToolCall{
						ID:   orDefault(item.CallID, orDefault(item.ID, "call_"+randHex(4))),
						Type: "function",
					}
					tc.Function.Name = item.Name
					tc.Function.Arguments = orDefault(item.Arguments, "{}")
					p := ensurePending()
					p.ToolCalls = append(p.ToolCalls, tc)
				case "function_call_output":
					flushPending()
					content := item.Output
					if len(content) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
						content = mustJSON("")
					}
					m := chatMessage{Role: "tool", ToolCallID: item.CallID}
					if len(content) > 0 && content[0] == '"' {
						m.Content = content
					} else {
						m.Content = mustJSON(string(content))
					}
					out.Messages = append(out.Messages, m)
				}
			}
		}
	}
	flushPending()

	if len(req.Tools) > 0 {
		tools := make([]chatTool, 0, len(req.Tools))
		for i := range req.Tools {
			t := &req.Tools[i]
			if t.Type != "function" && t.Name == "" {
				continue
			}
			tool := chatTool{Type: "function"}
			if t.Function != nil && t.Function.Name != "" {
				tool.Name = t.Function.Name
				tool.Description = t.Function.Description
				if len(t.Function.Parameters) > 0 {
					tool.Parameters = t.Function.Parameters
				}
			} else {
				tool.Name = t.Name
				tool.Description = t.Description
				if len(t.Parameters) > 0 {
					tool.Parameters = t.Parameters
				}
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			out.Tools = tools
		}
	}

	tc := bytes.TrimSpace(req.ToolChoice)
	if len(tc) > 0 && !bytes.Equal(tc, []byte("null")) {
		if tc[0] == '"' {
			out.ToolChoice = tc
		} else {
			var obj struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(tc, &obj); err == nil && obj.Name != "" {
				b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": obj.Name}})
				out.ToolChoice = b
			}
		}
	}
	return out
}

// ── Responses 对象 ──────────────────────────────────

// buildResponsesUsage：Responses 的 input_tokens 是总数，cached / cache_write 均为
// 其子集 —— 与 Anthropic 相反（那里 cache_read 是独立增量，必须做减法，见 issue #25）。
// 上游 CC 的 inputTokens 同样已含缓存，故此处直接沿用、不做减法。
func buildResponsesUsage(u *ccUsage, fallbackOutput int64) responsesUsage {
	if u == nil {
		u = &ccUsage{}
	}
	normalizeCCUsage(u)
	outTok := u.OutputTokens
	if outTok == 0 {
		outTok = fallbackOutput
	}
	return responsesUsage{
		InputTokens: u.InputTokens,
		InputTokensDetails: responsesInputTokenDetails{
			CachedTokens:     u.CachedInputTokens,
			CacheWriteTokens: u.cacheWrite(),
		},
		OutputTokens:        outTok,
		OutputTokensDetails: responsesOutputTokenDetails{ReasoningTokens: 0},
		TotalTokens:         u.InputTokens + outTok,
	}
}

type responsesInputTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type responsesOutputTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type responsesUsage struct {
	InputTokens         int64                       `json:"input_tokens"`
	InputTokensDetails  responsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int64                       `json:"output_tokens"`
	OutputTokensDetails responsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int64                       `json:"total_tokens"`
}

type responsesMessageItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type responsesReasoningItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Summary []any  `json:"summary"`
	Status  string `json:"status"`
}

type responsesFunctionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type responsesObject struct {
	ID                 string         `json:"id"`
	Object             string         `json:"object"`
	CreatedAt          int64          `json:"created_at"`
	Status             string         `json:"status"`
	CompletedAt        int64          `json:"completed_at"`
	Error              any            `json:"error"`
	IncompleteDetails  any            `json:"incomplete_details"`
	Input              any            `json:"input"`
	Instructions       any            `json:"instructions"`
	MaxOutputTokens    any            `json:"max_output_tokens"`
	Model              string         `json:"model"`
	Output             []any          `json:"output"`
	OutputText         string         `json:"output_text"`
	ParallelToolCalls  bool           `json:"parallel_tool_calls"`
	PreviousResponseID any            `json:"previous_response_id"`
	Reasoning          any            `json:"reasoning"`
	Store              bool           `json:"store"`
	Temperature        any            `json:"temperature"`
	Text               any            `json:"text"`
	ToolChoice         any            `json:"tool_choice"`
	Tools              any            `json:"tools"`
	TopP               any            `json:"top_p"`
	Truncation         string         `json:"truncation"`
	Usage              responsesUsage `json:"usage"`
	User               any            `json:"user"`
	Metadata           any            `json:"metadata"`
}

func buildResponsesOutput(fullText, thinkingText string, toolCalls []chatToolCall) []any {
	output := make([]any, 0, 3)
	if thinkingText != "" {
		output = append(output, responsesReasoningItem{
			Type:    "reasoning",
			ID:      newResponsesID("rs_"),
			Status:  "completed",
			Summary: []any{map[string]string{"type": "summary_text", "text": thinkingText}},
		})
	}
	if fullText != "" {
		output = append(output, responsesMessageItem{
			Type:   "message",
			ID:     newResponsesID("msg_"),
			Status: "completed",
			Role:   "assistant",
			Content: []any{map[string]any{
				"type": "output_text", "text": fullText, "annotations": []any{},
			}},
		})
	}
	for i := range toolCalls {
		tc := &toolCalls[i]
		args := orDefault(tc.Function.Arguments, "{}")
		output = append(output, responsesFunctionCallItem{
			Type:      "function_call",
			ID:        newResponsesID("fc_"),
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
			Status:    "completed",
		})
	}
	return output
}

// ── 非流式 ──────────────────────────────────────────

func (s *proxyServer) responsesNonStream(w http.ResponseWriter, resp *http.Response, req *responsesRequest,
	model, responseID string, created int64, start time.Time, path string) {
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
			s.log.Warn("CC error (Responses non-stream)", "message", mapped.Message)
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
				"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
			writeResponsesError(w, 429, "rate_limit_error", s.timeoutMessage(), 5)
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Upstream error", "message", readErr.Error(), "path", path)
		writeResponsesError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
		return
	}
	if upstreamErr != nil {
		writeResponsesError(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Message, upstreamErr.RetryAfter)
		return
	}
	if fullText.Len() == 0 && thinking.Len() == 0 && len(toolCalls) == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", false,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		writeResponsesError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}

	truncated := finishReason == "length"
	out := responsesObject{
		ID:                responseID,
		Object:            "response",
		CreatedAt:         created,
		Status:            map[bool]string{true: "incomplete", false: "completed"}[truncated],
		CompletedAt:       unixNow(),
		Error:             nil,
		Input:             []any{},
		Model:             model,
		Output:            buildResponsesOutput(fullText.String(), thinking.String(), toolCalls),
		OutputText:        fullText.String(),
		ParallelToolCalls: true,
		Reasoning:         req.Reasoning,
		Store:             false,
		Text:              map[string]any{"format": map[string]string{"type": "text"}},
		Tools:             orEmptySlice(req.Tools),
		Truncation:        "disabled",
		Usage:             buildResponsesUsage(usage, 0),
		User:              nil,
		Metadata:          map[string]any{},
	}
	if truncated {
		out.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
	}
	if req.Instructions != nil {
		out.Instructions = json.RawMessage(req.Instructions)
	}
	if req.MaxOutputTokens != nil {
		out.MaxOutputTokens = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out.Temperature = *req.Temperature
	} else {
		out.Temperature = 1
	}
	if req.TopP != nil {
		out.TopP = *req.TopP
	} else {
		out.TopP = 1
	}
	if tc := bytes.TrimSpace(req.ToolChoice); len(tc) > 0 && tc[0] == '"' {
		var s string
		if err := json.Unmarshal(tc, &s); err == nil {
			out.ToolChoice = s
		}
	} else {
		out.ToolChoice = "auto"
	}

	s.noteSuccess()
	writeJSON(w, 200, out, 0)
}

func orEmptySlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ── 流式 ────────────────────────────────────────────

type responsesItemState struct {
	kind      string // message | reasoning | function_call
	id        string
	callID    string
	name      string
	index     int64
	text      bytes.Buffer
	arguments bytes.Buffer
}

type responsesStream struct {
	seq          int64
	createdSent  bool
	current      *responsesItemState
	outputIndex  int64
	doneItems    []any
	textAcc      bytes.Buffer
	usage        *ccUsage
	finishReason string
	outputTokens int64
	hasContent   bool
	lastEvent    string
	bytes        int64
	lastWrite    time.Time
	upstreamErr  *mappedError
	model        string
	responseID   string
	created      int64
}

func (s *proxyServer) responsesStream(w http.ResponseWriter, resp *http.Response, req *responsesRequest,
	model, responseID string, created int64, start time.Time, path string) {
	sw := newSSEWriter(w, s.cfg.ClientStall)
	buf := getJSONBuf()
	defer putJSONBuf(buf)
	st := &responsesStream{lastWrite: start, model: model, responseID: responseID, created: created}
	var enc []byte

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
			if err := st.startResponse(sw, buf); err != nil {
				return err
			}
			if err := st.openItem(sw, buf, "message"); err != nil {
				return err
			}
			st.current.text.WriteString(text)
			st.textAcc.WriteString(text)
			buf.reset()
			beginResponsesEvent(buf, "response.output_text.delta", st.nextSeq())
			buf.raw(`"item_id":`).str(st.current.id).
				raw(`,"output_index":`).int(st.current.index).
				raw(`,"content_index":0,"delta":`).str(text).raw(`,"logprobs":[]}`)
			endFrame(buf)
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "reasoning-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.hasContent = true
			if err := st.startResponse(sw, buf); err != nil {
				return err
			}
			if err := st.openItem(sw, buf, "reasoning"); err != nil {
				return err
			}
			st.current.text.WriteString(text)
			buf.reset()
			beginResponsesEvent(buf, "response.reasoning_summary_text.delta", st.nextSeq())
			buf.raw(`"item_id":`).str(st.current.id).
				raw(`,"output_index":`).int(st.current.index).
				raw(`,"summary_index":0,"delta":`).str(text).byte('}')
			endFrame(buf)
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "tool-call":
			st.hasContent = true
			if err := st.startResponse(sw, buf); err != nil {
				return err
			}
			id := ev.ToolCallID
			if id == "" {
				id = newResponsesID("call_")
			}
			if err := st.openItemWith(sw, buf, "function_call", id, ev.ToolName); err != nil {
				return err
			}
			args := string(toolArgsJSON(ev.Input))
			st.current.arguments.WriteString(args)
			st.outputTokens += 20
			buf.reset()
			beginResponsesEvent(buf, "response.function_call_arguments.delta", st.nextSeq())
			buf.raw(`"item_id":`).str(st.current.id).
				raw(`,"output_index":`).int(st.current.index).
				raw(`,"delta":`).str(args).byte('}')
			endFrame(buf)
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "finish":
			st.finishReason = ev.FinishReason
			u := ev.TotalUsage
			if u == nil {
				u = ev.Usage
			}
			if u != nil {
				normalizeCCUsage(u)
				st.usage = u
				st.outputTokens = u.OutputTokens
			}
			return nil

		case "error":
			mapped := mapCCEventError(&ev)
			st.upstreamErr = &mapped
			s.log.Warn("CC stream error (Responses)", "message", mapped.Message)
			return nil

		case "text-start", "reasoning-start", "start", "start-step",
			"reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
			"tool-input-end", "tool-error", "text-end":
			if sw.Started() && time.Since(st.lastWrite) > 15*time.Second {
				st.lastWrite = time.Now()
				return sw.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
			}
			return nil

		default:
			return nil
		}
	})

	// 收尾：关闭当前 item（无论成功与否都要让下游拿到终止事件）
	if cerr := st.closeItem(sw, buf); cerr != nil && readErr == nil {
		readErr = cerr
	}

	if readErr != nil && !errors.Is(readErr, errClientGone) {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			msg := s.timeoutMessage()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", true,
				"timeoutMs", s.cfg.StreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent))
			if !sw.Started() {
				writeResponsesError(w, 429, "rate_limit_error", msg, 5)
				return
			}
			buf.reset()
			beginResponsesEvent(buf, "error", st.nextSeq())
			buf.raw(`"error":{"type":"rate_limit_error","code":null,"message":`).str(msg).raw(`,"param":null}}`)
			endFrame(buf)
			_ = sw.Write(buf.bytes())
			sw.Flush()
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Stream error", "message", readErr.Error(), "path", path)
		if !sw.Started() {
			writeResponsesError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
			return
		}
		buf.reset()
		beginResponsesEvent(buf, "error", st.nextSeq())
		buf.raw(`"error":{"type":"proxy_error","code":null,"message":`).str(readErr.Error()).raw(`,"param":null}}`)
		endFrame(buf)
		_ = sw.Write(buf.bytes())
		sw.Flush()
		return
	}

	if st.upstreamErr != nil {
		if !sw.Started() {
			e := *st.upstreamErr
			writeResponsesError(w, e.Status, e.Type, e.Message, e.RetryAfter)
			return
		}
		buf.reset()
		beginResponsesEvent(buf, "response.failed", st.nextSeq())
		buf.raw(`"response":`).rawJSON(marshalTo(&enc, st.baseResponse("failed", nil)))
		buf.raw(`,"error":{"code":"upstream_error","message":`).str(st.upstreamErr.Message).raw("}}")
		endFrame(buf)
		_ = sw.Write(buf.bytes())
		sw.Flush()
		return
	}

	if !st.hasContent && st.outputTokens == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", true,
			"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent))
		if !sw.Started() {
			writeResponsesError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
			return
		}
	}

	s.noteSuccess()
	if err := st.startResponse(sw, buf); err != nil {
		return
	}
	truncated := st.finishReason == "length"
	event := "response.completed"
	if truncated {
		event = "response.incomplete"
	}
	buf.reset()
	beginResponsesEvent(buf, event, st.nextSeq())
	buf.raw(`"response":`).rawJSON(marshalTo(&enc, st.baseResponse(map[bool]string{true: "incomplete", false: "completed"}[truncated], st.doneItems)))
	buf.raw(`,"output_text":`).str(st.textAcc.String())
	if truncated {
		buf.raw(`,"incomplete_details":{"reason":"max_output_tokens"}`)
	} else {
		buf.raw(`,"incomplete_details":null`)
	}
	buf.raw(`,"usage":`).rawJSON(marshalTo(&enc, buildResponsesUsage(st.usage, st.outputTokens)))
	buf.raw("}")
	endFrame(buf)
	_ = sw.Write(buf.bytes())
	sw.Flush()
}

func (st *responsesStream) nextSeq() int64 {
	s := st.seq
	st.seq++
	return s
}

// baseResponse 是 Responses 流式事件的 response 对象（非流式有更完整的字段集，
// 见 responsesObject）。这里用结构体 + json.Marshal，避免手写 JSON 与缓冲区别名。
type baseResponse struct {
	ID                 string `json:"id"`
	Object             string `json:"object"`
	CreatedAt          int64  `json:"created_at"`
	Status             string `json:"status"`
	Output             []any  `json:"output"`
	OutputText         string `json:"output_text"`
	Model              string `json:"model"`
	Error              any    `json:"error"`
	IncompleteDetails  any    `json:"incomplete_details"`
	ParallelToolCalls  bool   `json:"parallel_tool_calls"`
	PreviousResponseID any    `json:"previous_response_id"`
	Store              bool   `json:"store"`
	Tools              any    `json:"tools"`
	Metadata           any    `json:"metadata"`
}

func (st *responsesStream) baseResponse(status string, output []any) baseResponse {
	if output == nil {
		output = []any{}
	}
	return baseResponse{
		ID:                st.responseID,
		Object:            "response",
		CreatedAt:         st.created,
		Status:            status,
		Output:            output,
		Model:             st.model,
		ParallelToolCalls: true,
		Tools:             []any{},
		Metadata:          map[string]any{},
	}
}

func marshalTo(enc *[]byte, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("null")
	}
	*enc = b
	return b
}

func (st *responsesStream) startResponse(sw *sseWriter, buf *jsonBuf) error {
	if st.createdSent {
		return nil
	}
	st.createdSent = true
	var enc []byte

	buf.reset()
	beginResponsesEvent(buf, "response.created", st.nextSeq())
	buf.raw(`"response":`).rawJSON(marshalTo(&enc, st.baseResponse("in_progress", nil))).byte('}')
	endFrame(buf)
	if err := sw.Start(); err != nil {
		return err
	}
	if err := sw.Write(buf.bytes()); err != nil {
		return err
	}

	buf.reset()
	beginResponsesEvent(buf, "response.in_progress", st.nextSeq())
	buf.raw(`"response":`).rawJSON(marshalTo(&enc, st.baseResponse("in_progress", nil))).byte('}')
	endFrame(buf)
	return sw.Write(buf.bytes())
}

func (st *responsesStream) openItem(sw *sseWriter, buf *jsonBuf, kind string) error {
	return st.openItemWith(sw, buf, kind, "", "")
}

func (st *responsesStream) openItemWith(sw *sseWriter, buf *jsonBuf, kind, callID, name string) error {
	if st.current != nil && st.current.kind == kind &&
		(kind != "function_call" || st.current.callID == callID) {
		return nil
	}
	if err := st.closeItem(sw, buf); err != nil {
		return err
	}
	state := &responsesItemState{kind: kind, index: st.outputIndex}
	st.outputIndex++
	st.current = state

	buf.reset()
	switch kind {
	case "message":
		state.id = newResponsesID("msg_")
		beginResponsesEvent(buf, "response.output_item.added", st.nextSeq())
		buf.raw(`"output_index":`).int(state.index).
			raw(`,"item":{"type":"message","id":`).str(state.id).
			raw(`,"status":"in_progress","role":"assistant","content":[]}}`)
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		buf.reset()
		beginResponsesEvent(buf, "response.content_part.added", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
		endFrame(buf)
		return sw.Write(buf.bytes())
	case "reasoning":
		state.id = newResponsesID("rs_")
		beginResponsesEvent(buf, "response.output_item.added", st.nextSeq())
		buf.raw(`"output_index":`).int(state.index).
			raw(`,"item":{"type":"reasoning","id":`).str(state.id).
			raw(`,"summary":[],"status":"in_progress"}}`)
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		buf.reset()
		beginResponsesEvent(buf, "response.reasoning_summary_part.added", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		endFrame(buf)
		return sw.Write(buf.bytes())
	default: // function_call
		state.id = newResponsesID("fc_")
		state.callID = callID
		state.name = name
		beginResponsesEvent(buf, "response.output_item.added", st.nextSeq())
		buf.raw(`"output_index":`).int(state.index).
			raw(`,"item":{"type":"function_call","id":`).str(state.id).
			raw(`,"call_id":`).str(callID).
			raw(`,"name":`).str(name).
			raw(`,"arguments":"","status":"in_progress"}}`)
		endFrame(buf)
		return sw.Write(buf.bytes())
	}
}

func (st *responsesStream) closeItem(sw *sseWriter, buf *jsonBuf) error {
	if st.current == nil {
		return nil
	}
	state := st.current
	st.current = nil
	var item any
	switch state.kind {
	case "message":
		text := state.text.String()
		buf.reset()
		beginResponsesEvent(buf, "response.output_text.done", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"content_index":0,"text":`).str(text).raw(`,"logprobs":[]}`)
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		buf.reset()
		beginResponsesEvent(buf, "response.content_part.done", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"content_index":0,"part":{"type":"output_text","text":`).str(text).raw(`,"annotations":[]}}`)
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		item = responsesMessageItem{
			Type: "message", ID: state.id, Status: "completed", Role: "assistant",
			Content: []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
	case "reasoning":
		text := state.text.String()
		buf.reset()
		beginResponsesEvent(buf, "response.reasoning_summary_text.done", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"summary_index":0,"text":`).str(text).byte('}')
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		buf.reset()
		beginResponsesEvent(buf, "response.reasoning_summary_part.done", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"summary_index":0,"part":{"type":"summary_text","text":`).str(text).raw("}}")
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		item = responsesReasoningItem{
			Type: "reasoning", ID: state.id, Status: "completed",
			Summary: []any{map[string]string{"type": "summary_text", "text": text}},
		}
	default:
		args := state.arguments.String()
		buf.reset()
		beginResponsesEvent(buf, "response.function_call_arguments.done", st.nextSeq())
		buf.raw(`"item_id":`).str(state.id).
			raw(`,"output_index":`).int(state.index).
			raw(`,"arguments":`).str(args).byte('}')
		endFrame(buf)
		if err := sw.Write(buf.bytes()); err != nil {
			return err
		}
		item = responsesFunctionCallItem{
			Type: "function_call", ID: state.id, CallID: state.callID,
			Name: state.name, Arguments: args, Status: "completed",
		}
	}

	buf.reset()
	beginResponsesEvent(buf, "response.output_item.done", st.nextSeq())
	buf.raw(`"output_index":`).int(state.index).raw(`,"item":`)
	enc, _ := json.Marshal(item)
	buf.rawJSON(enc).byte('}')
	endFrame(buf)
	st.doneItems = append(st.doneItems, item)
	return sw.Write(buf.bytes())
}
