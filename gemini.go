package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── Google Gemini 兼容端点（/v1beta/models/{model}:generateContent） ──
//
// 供 Gemini SDK / 生态客户端接入。与其它端点一样是无状态翻译层：
// contents → 内部 Chat 表示 → CC；响应翻译回 GenerateContentResponse。
//
// 认证：x-goog-api-key 头或 ?key= 查询参数（extractPresentedKey 已支持）。
// 模型名从路径取，允许含斜杠（如 deepseek/deepseek-v4-flash），以最后一个
// `:` 分隔动作。

type geminiPart struct {
	Text string `json:"text"`
	// thought=true 表示这是思考摘要（Gemini thinking summary 语义）
	Thought bool `json:"thought,omitempty"`
	// 内联图片
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"` // base64
	} `json:"inlineData,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiTool struct {
	FunctionDeclarations []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"functionDeclarations"`
}

type geminiRequest struct {
	Contents               []geminiContent `json:"contents"`
	SystemInstruction      *geminiContent  `json:"systemInstruction"`
	SystemInstructionSnake *geminiContent  `json:"system_instruction"`
	GenerationConfig       *struct {
		MaxOutputTokens *int     `json:"maxOutputTokens"`
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"topP"`
		StopSequences   []string `json:"stopSequences"`
		ThinkingConfig  *struct {
			ThinkingBudget *int `json:"thinkingBudget"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
	Tools      []geminiTool `json:"tools"`
	ToolConfig *struct {
		FunctionCallingConfig *struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`
}

type geminiUsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
}

type geminiCandidate struct {
	Content       *geminiContent `json:"content,omitempty"`
	FinishReason  string         `json:"finishReason,omitempty"`
	Index         int            `json:"index"`
	SafetyRatings []any          `json:"safetyRatings,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion,omitempty"`
	ResponseID    string               `json:"responseId,omitempty"`
}

// parseGeminiPath 拆出模型名与动作（generateContent / streamGenerateContent）。
// 模型名可含斜杠，取最后一个 `:` 之后的部分作为动作。
func parseGeminiPath(path string) (model, action string, ok bool) {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	idx := strings.LastIndex(rest, ":")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

func geminiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"status":  httpStatusText(status),
		},
	}, 0)
}

func httpStatusText(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401, 403:
		return "UNAUTHENTICATED"
	case 404:
		return "NOT_FOUND"
	case 413:
		return "INVALID_ARGUMENT"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 502, 503:
		return "UNAVAILABLE"
	}
	return "UNKNOWN"
}

func (s *proxyServer) handleGemini(w http.ResponseWriter, r *http.Request, model, action string) {
	const path = "/v1beta"
	start := time.Now()

	streamAction := action == "streamGenerateContent"
	if action != "generateContent" && !streamAction {
		geminiError(w, 404, fmt.Sprintf("unsupported action %q (supported: generateContent, streamGenerateContent)", action))
		return
	}
	// streamGenerateContent?alt=sse 为 SSE；无 alt=sse 时返回 JSON 数组
	altSSE := r.URL.Query().Get("alt") == "sse"

	body, release, err := readBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			geminiError(w, 413, s.bodyLimitMessage())
			return
		}
		geminiError(w, 400, "Invalid JSON body")
		return
	}
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		release()
		geminiError(w, 400, "Invalid JSON body")
		return
	}
	release()

	ar, err := s.authorize(r)
	if err != nil {
		e := authFailure(err)
		geminiError(w, e.Status, e.Message)
		return
	}

	chatReq := convertGeminiToChat(&req, model)
	if len(chatReq.Messages) == 0 {
		geminiError(w, 400, "contents is required")
		return
	}
	if chatReq.Model == "" {
		chatReq.Model = defaultChatModel
	}
	model = chatReq.Model

	ccBody, err := buildCCBody(s.cfg, chatReq)
	if err != nil {
		geminiError(w, 500, "Failed to build upstream request")
		return
	}

	sessionID := sessionFromHeaders(r, chatReq.PromptCacheKey)
	if sessionID == "" {
		sessionID = ar.upstream.sessionIDFor(time.Now())
	}

	resp, usedKey, err := s.generateWithFailover(r.Context(), ar, ccBody, sessionID,
		s.cc.projectSlugFor(sessionID), streamAction, wantsZDR(r, s.cfg.ZDR))
	if err != nil {
		if upstreamCancelled(err) {
			s.log.Warn("Request cancelled before CC response", "path", path, "model", model, "elapsedMs", sinceMs(start))
			return
		}
		s.log.Error("Upstream error", "message", err.Error(), "path", path)
		geminiError(w, 502, "Upstream error: "+err.Error())
		return
	}
	if resp.StatusCode/100 != 2 {
		mapped := mapCCStatus(resp.StatusCode, readErrorBody(resp))
		s.noteUpstreamModelRejection(model, mapped.Message)
		s.log.Error("CC API error (Gemini)", "status", resp.StatusCode, "key", usedKey.label())
		geminiError(w, mapped.Status, mapped.Message)
		return
	}
	defer drainAndClose(resp.Body)

	responseID := newResponsesID("gmn_")
	switch {
	case streamAction && altSSE:
		s.geminiStreamSSE(w, resp, model, responseID, start, path)
	case streamAction:
		s.geminiStreamArray(w, resp, model, responseID, start, path)
	default:
		s.geminiNonStream(w, resp, model, responseID, start, path)
	}
}

// ── Gemini → 内部 Chat 表示 ─────────────────────────

func geminiPartsText(parts []geminiPart) string {
	var sb bytes.Buffer
	for i := range parts {
		sb.WriteString(parts[i].Text)
	}
	return sb.String()
}

func convertGeminiToChat(req *geminiRequest, model string) *chatRequest {
	out := &chatRequest{Model: model}
	if req.GenerationConfig != nil {
		gc := req.GenerationConfig
		out.MaxTokens = gc.MaxOutputTokens
		out.Temperature = gc.Temperature
		out.TopP = gc.TopP
		if len(gc.StopSequences) > 0 {
			out.Stop = mustJSON(gc.StopSequences)
		}
		if gc.ThinkingConfig != nil && gc.ThinkingConfig.ThinkingBudget != nil {
			// 与 Anthropic budget_tokens 同一套映射：≥10000→high, ≥5000→medium, ≥2000→low
			b := *gc.ThinkingConfig.ThinkingBudget
			switch {
			case b >= 10000:
				out.ReasoningEffort = "high"
			case b >= 5000:
				out.ReasoningEffort = "medium"
			case b > 0:
				out.ReasoningEffort = "low"
			}
		}
	}

	sys := req.SystemInstruction
	if sys == nil {
		sys = req.SystemInstructionSnake
	}
	if sys != nil {
		if text := geminiPartsText(sys.Parts); text != "" {
			out.Messages = append(out.Messages, chatMessage{Role: "system", Content: mustJSON(text)})
		}
	}

	// functionCall / functionResponse 靠 name 配对（Gemini 协议没有调用 id）
	lastCallID := map[string]string{}
	var pending *chatMessage
	flushPending := func() {
		if pending == nil {
			return
		}
		if pending.Content == nil && len(pending.ToolCalls) == 0 {
			pending = nil
			return
		}
		out.Messages = append(out.Messages, *pending)
		pending = nil
	}

	for _, c := range req.Contents {
		role := "user"
		if c.Role == "model" {
			role = "assistant"
		}
		for _, p := range c.Parts {
			switch {
			case p.InlineData != nil:
				flushPending()
				dataURL := "data:" + p.InlineData.MimeType + ";base64," + p.InlineData.Data
				content := mustJSON([]map[string]any{{
					"type":      "image_url",
					"image_url": map[string]string{"url": dataURL},
				}})
				out.Messages = append(out.Messages, chatMessage{Role: "user", Content: content})

			case p.FunctionCall != nil:
				if pending == nil {
					pending = &chatMessage{Role: "assistant"}
				}
				id := "call_" + randHex(6)
				lastCallID[p.FunctionCall.Name] = id
				tc := chatToolCall{ID: id, Type: "function"}
				tc.Function.Name = p.FunctionCall.Name
				tc.Function.Arguments = string(p.FunctionCall.Args)
				pending.ToolCalls = append(pending.ToolCalls, tc)

			case p.FunctionResponse != nil:
				flushPending()
				// tool_call_id 用配对的 functionCall id；找不到就按 name 造一个
				id := lastCallID[p.FunctionResponse.Name]
				if id == "" {
					id = "call_" + randHex(6)
				}
				respText := string(p.FunctionResponse.Response)
				out.Messages = append(out.Messages, chatMessage{
					Role:       "tool",
					ToolCallID: id,
					Content:    mustJSON(respText),
				})

			default:
				if p.Text == "" {
					continue
				}
				if role == "assistant" {
					if pending == nil {
						pending = &chatMessage{Role: "assistant"}
					}
					if p.Thought {
						pending.ReasoningContent += p.Text
					} else {
						pending.Content = mustJSON(geminiPartsText([]geminiPart{p}))
					}
				} else {
					flushPending()
					out.Messages = append(out.Messages, chatMessage{Role: "user", Content: mustJSON(p.Text)})
				}
			}
		}
	}
	flushPending()

	for _, t := range req.Tools {
		for _, fd := range t.FunctionDeclarations {
			tool := chatTool{Type: "function"}
			tool.Function = &struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
				Strict      *bool           `json:"strict"`
			}{Name: fd.Name, Description: fd.Description, Parameters: fd.Parameters}
			out.Tools = append(out.Tools, tool)
		}
	}

	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		fcc := req.ToolConfig.FunctionCallingConfig
		switch strings.ToUpper(fcc.Mode) {
		case "ANY":
			if len(fcc.AllowedFunctionNames) == 1 {
				b, _ := json.Marshal(map[string]any{
					"type":     "function",
					"function": map[string]string{"name": fcc.AllowedFunctionNames[0]},
				})
				out.ToolChoice = b
			} else {
				out.ToolChoice = mustJSON("required")
			}
		case "NONE":
			out.ToolChoice = mustJSON("none")
		default:
			out.ToolChoice = mustJSON("auto")
		}
	}
	return out
}

// ── 内部结果 → Gemini 响应 ──────────────────────────

// mapGeminiFinishReason：tool_calls 在 Gemini 里以 functionCall part 表达，
// 终止原因仍是 STOP。
func mapGeminiFinishReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "MAX_TOKENS"
	case "":
		return "STOP"
	default:
		return "STOP"
	}
}

func buildGeminiUsage(u *ccUsage, fallbackOutput int64) *geminiUsageMetadata {
	if u == nil {
		u = &ccUsage{}
	}
	normalizeCCUsage(u)
	outTok := u.OutputTokens
	if outTok == 0 {
		outTok = fallbackOutput
	}
	// Gemini 的 promptTokenCount 是总数（含缓存），与 CC 语义一致，无需减法
	return &geminiUsageMetadata{
		PromptTokenCount:        u.InputTokens,
		CandidatesTokenCount:    outTok,
		TotalTokenCount:         u.InputTokens + outTok,
		CachedContentTokenCount: u.CachedInputTokens,
	}
}

func buildGeminiCandidates(fullText, thinking string, toolCalls []chatToolCall, finishReason string) []geminiCandidate {
	parts := make([]geminiPart, 0, 3)
	if thinking != "" {
		parts = append(parts, geminiPart{Text: thinking, Thought: true})
	}
	if fullText != "" {
		parts = append(parts, geminiPart{Text: fullText})
	}
	for i := range toolCalls {
		tc := &toolCalls[i]
		fc := &struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}{Name: tc.Function.Name}
		if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
			fc.Args = json.RawMessage(args)
		} else {
			fc.Args = json.RawMessage("{}")
		}
		p := geminiPart{Text: fullText}
		p.FunctionCall = fc
		p.Text = ""
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		parts = append(parts, geminiPart{Text: ""})
	}
	return []geminiCandidate{{
		Content:      &geminiContent{Role: "model", Parts: parts},
		FinishReason: mapGeminiFinishReason(finishReason),
		Index:        0,
	}}
}

// ── 非流式 ──────────────────────────────────────────

func (s *proxyServer) geminiNonStream(w http.ResponseWriter, resp *http.Response,
	model, responseID string, start time.Time, path string) {
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
			finishReason = ev.FinishReason
			u := ev.TotalUsage
			if u == nil {
				u = ev.Usage
			}
			if u != nil {
				usage = u
			}
		case "error":
			mapped := mapCCEventError(&ev)
			upstreamErr = &mapped
			s.log.Warn("CC error (Gemini non-stream)", "message", mapped.Message)
		case "reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
			"tool-input-end", "tool-error", "text-end":
			// 静默
		}
		return nil
	})

	if readErr != nil {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", false,
				"timeoutMs", s.cfg.NonstreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
			geminiError(w, 429, s.timeoutMessage())
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Upstream error", "message", readErr.Error(), "path", path)
		geminiError(w, 502, "Upstream error: "+readErr.Error())
		return
	}
	if upstreamErr != nil {
		geminiError(w, upstreamErr.Status, upstreamErr.Message)
		return
	}
	if fullText.Len() == 0 && thinking.Len() == 0 && len(toolCalls) == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", false,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		geminiError(w, 429, "Empty response from upstream (zero output tokens)")
		return
	}

	s.noteSuccess()
	writeJSON(w, 200, geminiResponse{
		Candidates:    buildGeminiCandidates(fullText.String(), thinking.String(), toolCalls, finishReason),
		UsageMetadata: buildGeminiUsage(usage, 0),
		ModelVersion:  model,
		ResponseID:    responseID,
	}, 0)
}

// ── 流式（alt=sse → SSE；否则 JSON 数组） ──────────

type geminiStreamState struct {
	usage      *ccUsage
	finish     string
	hasContent bool
	outputTok  int64
	lastWrite  time.Time
}

func (st *geminiStreamState) chunk(text string, thought bool, tc *struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}) geminiResponse {
	parts := make([]geminiPart, 0, 1)
	if tc != nil {
		p := geminiPart{}
		p.FunctionCall = tc
		parts = append(parts, p)
	} else {
		parts = append(parts, geminiPart{Text: text, Thought: thought})
	}
	c := geminiCandidate{Content: &geminiContent{Role: "model", Parts: parts}, Index: 0}
	if st.finish != "" {
		c.FinishReason = mapGeminiFinishReason(st.finish)
	}
	return geminiResponse{Candidates: []geminiCandidate{c}, UsageMetadata: buildGeminiUsage(st.usage, st.outputTok)}
}

func (st *geminiStreamState) consume(ev *ccEvent) {
	switch ev.Type {
	case "text-delta":
		st.hasContent = true
		st.outputTok++
	case "reasoning-delta":
		st.hasContent = true
		st.outputTok++
	case "tool-call":
		st.hasContent = true
		st.outputTok += 20
	case "finish":
		st.finish = ev.FinishReason
		u := ev.TotalUsage
		if u == nil {
			u = ev.Usage
		}
		if u != nil {
			normalizeCCUsage(u)
			st.usage = u
			st.outputTok = u.OutputTokens
		}
	}
}

func (s *proxyServer) geminiStreamSSE(w http.ResponseWriter, resp *http.Response,
	model, responseID string, start time.Time, path string) {
	sw := newSSEWriter(w, s.cfg.ClientStall)
	st := &geminiStreamState{lastWrite: start}

	var (
		upstreamErr *mappedError
		lastEvent   string
		bytesRecv   int64
	)

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

		var frame *geminiResponse
		switch ev.Type {
		case "text-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.consume(&ev)
			ch := st.chunk(text, false, nil)
			frame = &ch
		case "reasoning-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.consume(&ev)
			ch := st.chunk(text, true, nil)
			frame = &ch
		case "tool-call":
			st.consume(&ev)
			fc := &struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}{Name: ev.ToolName, Args: toolArgsJSON(ev.Input)}
			ch := st.chunk("", false, fc)
			frame = &ch
		case "finish":
			st.consume(&ev)
			// 终帧：带 finishReason + 最终 usage
			ch := st.chunk("", false, nil)
			frame = &ch
		case "error":
			mapped := mapCCEventError(&ev)
			upstreamErr = &mapped
			s.log.Warn("CC stream error (Gemini)", "message", mapped.Message)
			return nil
		default:
			return nil
		}

		if err := sw.Start(); err != nil {
			return err
		}
		enc, err := json.Marshal(frame)
		if err != nil {
			return nil
		}
		st.lastWrite = time.Now()
		return sw.Write([]byte("data: " + string(enc) + "\n\n"))
	})

	if readErr != nil && !errors.Is(readErr, errClientGone) {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", true,
				"timeoutMs", s.cfg.StreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
			if !sw.Started() {
				geminiError(w, 429, s.timeoutMessage())
				return
			}
			_ = sw.Write([]byte("data: {\"error\":{\"code\":429,\"message\":\"" + s.timeoutMessage() + "\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n"))
			sw.Flush()
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Stream error", "message", readErr.Error(), "path", path)
		if !sw.Started() {
			geminiError(w, 502, "Upstream error: "+readErr.Error())
			return
		}
		_ = sw.Write([]byte("data: {\"error\":{\"code\":502,\"message\":\"upstream error\",\"status\":\"UNAVAILABLE\"}}\n\n"))
		sw.Flush()
		return
	}
	if upstreamErr != nil {
		if !sw.Started() {
			geminiError(w, upstreamErr.Status, upstreamErr.Message)
			return
		}
		msg, _ := json.Marshal(upstreamErr.Message)
		_ = sw.Write([]byte("data: {\"error\":{\"code\":" + itoa(upstreamErr.Status) + ",\"message\":" + string(msg) + ",\"status\":\"" + httpStatusText(upstreamErr.Status) + "\"}}\n\n"))
		sw.Flush()
		return
	}
	if !st.hasContent && st.outputTok == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", true,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		if !sw.Started() {
			geminiError(w, 429, "Empty response from upstream (zero output tokens)")
			return
		}
	}

	s.noteSuccess()
	if !sw.Started() {
		// 上游没有任何内容事件：输出一个仅含 usage 的终帧
		ch := st.chunk("", false, nil)
		enc, _ := json.Marshal(&ch)
		_ = sw.Start()
		_ = sw.Write([]byte("data: " + string(enc) + "\n\n"))
	}
	sw.Flush()
}

func (s *proxyServer) geminiStreamArray(w http.ResponseWriter, resp *http.Response,
	model, responseID string, start time.Time, path string) {
	var (
		chunks      []geminiResponse
		upstreamErr *mappedError
		lastEvent   string
		bytesRecv   int64
	)
	st := &geminiStreamState{}

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
			if ev.textOf() == "" {
				return nil
			}
			st.consume(&ev)
			ch := st.chunk(ev.textOf(), false, nil)
			chunks = append(chunks, ch)
		case "reasoning-delta":
			if ev.textOf() == "" {
				return nil
			}
			st.consume(&ev)
			ch := st.chunk(ev.textOf(), true, nil)
			chunks = append(chunks, ch)
		case "tool-call":
			st.consume(&ev)
			fc := &struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}{Name: ev.ToolName, Args: toolArgsJSON(ev.Input)}
			ch := st.chunk("", false, fc)
			chunks = append(chunks, ch)
		case "finish":
			st.consume(&ev)
		case "error":
			mapped := mapCCEventError(&ev)
			upstreamErr = &mapped
			s.log.Warn("CC stream error (Gemini array)", "message", mapped.Message)
		}
		return nil
	})

	if readErr != nil {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", true,
				"timeoutMs", s.cfg.StreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
			geminiError(w, 429, s.timeoutMessage())
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Stream error", "message", readErr.Error(), "path", path)
		geminiError(w, 502, "Upstream error: "+readErr.Error())
		return
	}
	if upstreamErr != nil {
		geminiError(w, upstreamErr.Status, upstreamErr.Message)
		return
	}
	if len(chunks) == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", true,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		geminiError(w, 429, "Empty response from upstream (zero output tokens)")
		return
	}
	s.noteSuccess()
	// 终帧带 finishReason + 最终 usage
	last := st.chunk("", false, nil)
	chunks = append(chunks, last)
	writeJSON(w, 200, chunks, 0)
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}
