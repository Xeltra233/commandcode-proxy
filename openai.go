package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const defaultChatModel = "deepseek/deepseek-v4-flash"

var doneLiteral = []byte("[DONE]")

// handleChatCompletions 是 OpenAI /v1/chat/completions 端点。
func (s *proxyServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	const path = "/v1/chat/completions"
	start := time.Now()

	body, release, err := readBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOpenAIError(w, 413, "invalid_request_error", s.bodyLimitMessage(), 0)
			return
		}
		writeOpenAIError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		release()
		writeOpenAIError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	release()

	ar, err := s.authorize(r)
	if err != nil {
		e := authFailure(err)
		writeOpenAIError(w, e.Status, e.Type, e.Message, 0)
		return
	}

	stream := req.Stream
	model := req.Model
	if model == "" {
		model = defaultChatModel
	}

	ccBody, err := buildCCBody(s.cfg, &req)
	if err != nil {
		writeOpenAIError(w, 500, "internal_error", "Failed to build upstream request", 0)
		return
	}

	sessionID := sessionFromHeaders(r, req.PromptCacheKey)
	if sessionID == "" {
		sessionID = ar.upstream.sessionIDFor(time.Now())
	}

	resp, usedKey, err := s.generateWithFailover(r.Context(), ar, ccBody, sessionID,
		s.cc.projectSlugFor(sessionID), stream, wantsZDR(r, s.cfg.ZDR))
	if err != nil {
		if upstreamCancelled(err) {
			s.log.Warn("Request cancelled before CC response", "path", path, "model", model, "elapsedMs", sinceMs(start))
			return
		}
		s.log.Error("Upstream error", "message", err.Error(), "path", path, "model", model)
		writeOpenAIError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
		return
	}
	if resp.StatusCode/100 != 2 {
		mapped := mapCCStatus(resp.StatusCode, readErrorBody(resp))
		s.noteUpstreamModelRejection(model, mapped.Message)
		s.log.Error("CC API error", "status", resp.StatusCode, "path", path, "key", usedKey.label())
		writeOpenAIError(w, mapped.Status, mapped.Type, mapped.Message, mapped.RetryAfter)
		return
	}
	defer drainAndClose(resp.Body)

	completionID := "chatcmpl-" + randHex(6)
	created := unixNow()
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	if stream {
		s.chatStream(w, r, resp, model, completionID, created, start, includeUsage)
	} else {
		s.chatNonStream(w, resp, model, completionID, created, start, path)
	}
}

// ── 流式（NDJSON → OpenAI SSE） ─────────────────────

type openAIStream struct {
	chunkIndex    int64
	toolCallIndex int64
	sentRole      bool
	finishReason  string
	usage         *ccUsage
	lastEvent     string
	bytes         int64
	hasContent    bool
	lastWrite     time.Time
	upstreamErr   *mappedError
}

func (s *proxyServer) chatStream(w http.ResponseWriter, r *http.Request, resp *http.Response,
	model, completionID string, created int64, start time.Time, includeUsage bool) {
	const path = "/v1/chat/completions"

	sw := newSSEWriter(w, s.cfg.ClientStall)
	st := &openAIStream{lastWrite: start}
	buf := getJSONBuf()
	defer putJSONBuf(buf)

	readErr := readCCLines(resp.Body, func(line []byte) error {
		st.bytes += int64(len(line)) + 1
		if len(line) == 0 || line[0] == ':' || bytes.Equal(line, doneLiteral) {
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
			if err := sw.Start(); err != nil {
				return err
			}
			buf.reset()
			beginOpenAIChunk(buf, completionID, model, created)
			if !st.sentRole {
				st.sentRole = true
				buf.raw(`{"role":"assistant","content":`).str(text).byte('}')
			} else {
				buf.raw(`{"content":`).str(text).byte('}')
			}
			endOpenAIChunk(buf, "")
			finishOpenAIChunk(buf)
			st.chunkIndex++
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "reasoning-delta":
			text := ev.textOf()
			if text == "" {
				return nil
			}
			st.hasContent = true
			if err := sw.Start(); err != nil {
				return err
			}
			buf.reset()
			beginOpenAIChunk(buf, completionID, model, created)
			if !st.sentRole {
				st.sentRole = true
				buf.raw(`{"role":"assistant","reasoning_content":`).str(text).byte('}')
			} else {
				buf.raw(`{"reasoning_content":`).str(text).byte('}')
			}
			endOpenAIChunk(buf, "")
			finishOpenAIChunk(buf)
			st.chunkIndex++
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "tool-call":
			st.hasContent = true
			if err := sw.Start(); err != nil {
				return err
			}
			id := ev.ToolCallID
			if id == "" {
				id = "call_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + strconv.FormatInt(st.toolCallIndex, 10)
			}
			buf.reset()
			beginOpenAIChunk(buf, completionID, model, created)
			if !st.sentRole {
				st.sentRole = true
				buf.raw(`{"role":"assistant","content":null,"tool_calls":[`)
			} else {
				buf.raw(`{"tool_calls":[`)
			}
			buf.raw(`{"index":`).int(st.toolCallIndex).
				raw(`,"id":`).str(id).
				raw(`,"type":"function","function":{"name":`).str(ev.ToolName).
				raw(`,"arguments":`).str(string(toolArgsJSON(ev.Input))).raw("}}]}")
			endOpenAIChunk(buf, "")
			finishOpenAIChunk(buf)
			st.toolCallIndex++
			st.chunkIndex++
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "finish-step":
			if ev.FinishReason != "" {
				st.finishReason = mapFinishReason(ev.FinishReason)
			}
			if ev.Usage != nil {
				normalizeCCUsage(ev.Usage)
				st.usage = ev.Usage
			}
			return nil

		case "finish":
			fr := st.finishReason
			if fr == "" {
				fr = mapFinishReason(ev.FinishReason)
			}
			u := ev.TotalUsage
			if u == nil {
				u = ev.Usage
			}
			if u == nil {
				u = st.usage
			}
			if u == nil {
				u = &ccUsage{}
			}
			normalizeCCUsage(u)
			st.usage = u
			if err := sw.Start(); err != nil {
				return err
			}
			buf.reset()
			beginOpenAIChunk(buf, completionID, model, created)
			buf.raw("{}")
			endOpenAIChunk(buf, fr)
			// include_usage 语义：usage 不进 choices chunk，单独发一个 choices 为空的收尾 chunk
			if !includeUsage {
				appendOpenAIUsage(buf, u.InputTokens, u.OutputTokens, u.CachedInputTokens)
			}
			finishOpenAIChunk(buf)
			st.lastWrite = time.Now()
			return sw.Write(buf.bytes())

		case "error":
			mapped := mapCCEventError(&ev)
			st.upstreamErr = &mapped
			s.log.Warn("CC stream error", "path", path, "model", model, "key", "n/a", "message", mapped.Message)
			return nil

		case "text-start", "reasoning-start", "start", "start-step",
			"reasoning-end", "provider-metadata", "tool-input-start", "tool-input-delta",
			"tool-input-end", "tool-error", "text-end":
			// 静默事件：给已经开始的流发心跳注释行，避免下游首字节/空闲超时
			if sw.Started() && time.Since(st.lastWrite) > 15*time.Second {
				st.lastWrite = time.Now()
				return sw.Write([]byte(": keepalive\n\n"))
			}
			return nil

		default:
			s.log.Warn("Unknown CC event type", "type", ev.Type)
			return nil
		}
	})

	if readErr != nil && !errors.Is(readErr, errClientGone) {
		if errors.Is(readErr, errUpstreamIdle) {
			s.noteTimeout()
			s.log.Warn("Stream idle timeout", "path", path, "model", model, "streaming", true,
				"timeoutMs", s.cfg.StreamIdle.Milliseconds(), "elapsedMs", sinceMs(start),
				"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent))
			// 首帧之前（尚未 Start）必须回 JSON 错误：否则 handler 零输出退出，
			// net/http 默认回 200 + 空流，下游只能看到"流意外结束"（2026-09-17 glm 线上事故：
			// new-api 记 end_reason=eof/ok，pi 报 Stream ended without finish_reason）。
			if !sw.Started() {
				writeOpenAIError(w, 429, "rate_limit_error", s.timeoutMessage(), 5)
				return
			}
			if err := s.idleErrorFrame(sw, buf, s.timeoutMessage(), 5); err != nil {
				return
			}
			sw.Flush()
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Stream error", "message", readErr.Error(), "path", path, "model", model)
		if !sw.Started() {
			writeOpenAIError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
			return
		}
		_ = s.streamErrorFrame(sw, buf, "proxy_error", readErr.Error(), 0)
		sw.Flush()
		return
	}

	if st.upstreamErr != nil {
		if !sw.Started() {
			e := *st.upstreamErr
			writeOpenAIError(w, e.Status, e.Type, e.Message, e.RetryAfter)
			return
		}
		buf.reset()
		buf.raw("data: ").raw(`{"error":{"message":`).str(st.upstreamErr.Message).
			raw(`,"type":`).str(st.upstreamErr.Type).raw("}")
		if st.upstreamErr.RetryAfter > 0 {
			buf.raw(`,"retry_after":`).int(int64(st.upstreamErr.RetryAfter))
		}
		buf.raw("}\n\n")
		_ = sw.Write(buf.bytes())
		sw.Flush()
		return
	}

	// 零输出保护：上游返回 200 却没有任何内容时按 429 处理，避免下游异常计费
	if !st.hasContent && st.usage == nil {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", true,
			"bytesReceived", st.bytes, "lastCcEvent", orNone(st.lastEvent))
		if !sw.Started() {
			writeOpenAIError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
			return
		}
	}

	s.noteSuccess()
	if includeUsage && st.usage != nil {
		if err := sw.Start(); err != nil {
			return
		}
		u := st.usage
		buf.reset()
		buf.raw(`data: {"id":`).str(completionID).
			raw(`,"object":"chat.completion.chunk","created":`).int(created).
			raw(`,"model":`).str(model).
			raw(`,"choices":[],"usage":{"prompt_tokens":`).int(u.InputTokens).
			raw(`,"completion_tokens":`).int(u.OutputTokens).
			raw(`,"total_tokens":`).int(u.InputTokens + u.OutputTokens).
			raw(`,"prompt_tokens_details":{"cached_tokens":`).int(u.CachedInputTokens).
			raw("}}}\n\n")
		_ = sw.Write(buf.bytes())
	}
	if err := sw.Start(); err != nil {
		return
	}
	_ = sw.Write([]byte("data: [DONE]\n\n"))
	sw.Flush()
}

// streamErrorFrame 在流已经开始的情况下，用 SSE 数据帧回传错误（而不是改状态码）。
func (s *proxyServer) streamErrorFrame(sw *sseWriter, buf *jsonBuf, typ, msg string, retryAfter int) error {
	if !sw.Started() {
		return errors.New("stream not started")
	}
	buf.reset()
	buf.raw(`data: {"error":{"message":`).str(msg).raw(`,"type":`).str(typ).raw("}")
	if retryAfter > 0 {
		buf.raw(`,"retry_after":`).int(int64(retryAfter))
	}
	buf.raw("}\n\n")
	return sw.Write(buf.bytes())
}

func (s *proxyServer) idleErrorFrame(sw *sseWriter, buf *jsonBuf, msg string, retryAfter int) error {
	return s.streamErrorFrame(sw, buf, "rate_limit_error", msg, retryAfter)
}

// ── 非流式 ──────────────────────────────────────────

type chatCompletionMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int                   `json:"index"`
		Message      chatCompletionMessage `json:"message"`
		FinishReason string                `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

func (s *proxyServer) chatNonStream(w http.ResponseWriter, resp *http.Response,
	model, completionID string, created int64, start time.Time, path string) {
	var (
		fullText    bytes.Buffer
		reasoning   bytes.Buffer
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
			reasoning.WriteString(ev.textOf())
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
			s.log.Warn("CC error (non-stream)", "path", path, "message", mapped.Message)
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
			writeOpenAIError(w, 429, "rate_limit_error", s.timeoutMessage(), 5)
			return
		}
		if upstreamCancelled(readErr) {
			return
		}
		s.log.Error("Upstream error", "message", readErr.Error(), "path", path)
		writeOpenAIError(w, 502, "proxy_error", "Upstream error: "+readErr.Error(), 10)
		return
	}
	if upstreamErr != nil {
		writeOpenAIError(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Message, upstreamErr.RetryAfter)
		return
	}
	if fullText.Len() == 0 && reasoning.Len() == 0 && len(toolCalls) == 0 {
		s.log.Warn("Zero output from upstream", "path", path, "model", model, "streaming", false,
			"bytesReceived", bytesRecv, "lastCcEvent", orNone(lastEvent))
		writeOpenAIError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}

	msg := chatCompletionMessage{Role: "assistant"}
	if text := fullText.String(); text != "" {
		msg.Content = &text
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	if r := reasoning.String(); r != "" {
		msg.ReasoningContent = r
	}

	var out chatCompletionResponse
	out.ID = completionID
	out.Object = "chat.completion"
	out.Created = created
	out.Model = model
	out.Choices = make([]struct {
		Index        int                   `json:"index"`
		Message      chatCompletionMessage `json:"message"`
		FinishReason string                `json:"finish_reason"`
	}, 1)
	out.Choices[0].Index = 0
	out.Choices[0].Message = msg
	out.Choices[0].FinishReason = finishReason

	u := usage
	if u == nil {
		u = &ccUsage{}
	}
	normalizeCCUsage(u)
	// OpenAI 语义：prompt_tokens 是总数（含缓存命中），与 Anthropic 相反
	out.Usage.PromptTokens = u.InputTokens
	out.Usage.CompletionTokens = u.OutputTokens
	out.Usage.TotalTokens = u.InputTokens + u.OutputTokens
	out.Usage.PromptTokensDetails.CachedTokens = u.CachedInputTokens

	s.noteSuccess()
	writeJSON(w, 200, out, 0)
}

func (s *proxyServer) bodyLimitMessage() string {
	return "Request body exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes>>20, 10) + "MB limit"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
