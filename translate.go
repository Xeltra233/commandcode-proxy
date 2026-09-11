package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// ── 上游 NDJSON 事件 ────────────────────────────────

type ccInputTokenDetails struct {
	NoCacheTokens    *int64 `json:"noCacheTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
}

type ccUsage struct {
	InputTokens       int64                `json:"inputTokens"`
	OutputTokens      int64                `json:"outputTokens"`
	CachedInputTokens int64                `json:"cachedInputTokens"`
	InputTokenDetails *ccInputTokenDetails `json:"inputTokenDetails"`
}

type ccEventError struct {
	Message string `json:"message"`
}

type ccEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Delta        string          `json:"delta"`
	FinishReason string          `json:"finishReason"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	Input        json.RawMessage `json:"input"`
	Usage        *ccUsage        `json:"usage"`
	TotalUsage   *ccUsage        `json:"totalUsage"`
	Error        *ccEventError   `json:"error"`
	Message      string          `json:"message"`
}

// textOf 兼容上游把增量放在 text 或 delta 两种字段里。
func (e *ccEvent) textOf() string {
	if e.Text != "" {
		return e.Text
	}
	return e.Delta
}

func (e *ccEvent) errorMessage() string {
	if e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if e.Message != "" {
		return e.Message
	}
	return "Unknown CC error"
}

// ── usage 归一化 ────────────────────────────────────

// normalizeCCUsage 把 outputTokens=0 的响应整体归零：上游偶发返回
// {inputTokens: N, outputTokens: 0} 时，直接转发会让下游按输入量计费却没有输出。
func normalizeCCUsage(u *ccUsage) {
	if u == nil {
		return
	}
	if u.OutputTokens == 0 {
		u.InputTokens = 0
		u.CachedInputTokens = 0
	}
}

func (u *ccUsage) cacheWrite() int64 {
	if u == nil || u.InputTokenDetails == nil {
		return 0
	}
	return u.InputTokenDetails.CacheWriteTokens
}

func (u *ccUsage) cacheRead() int64 {
	if u == nil {
		return 0
	}
	if u.CachedInputTokens != 0 {
		return u.CachedInputTokens
	}
	if u.InputTokenDetails != nil {
		return u.InputTokenDetails.CacheReadTokens
	}
	return 0
}

// anthropicInputTokens 返回 Anthropic 语义的 input_tokens（只计非缓存部分）。
//
// CC 的 inputTokens 是「总数」（含缓存命中），而 Anthropic 的 input_tokens 只计非缓存
// 部分 —— 官方 SDK 注释：Total input tokens in a request is the summation of
// `input_tokens`, `cache_creation_input_tokens`, and `cache_read_input_tokens`。
// 直接把总数当 input_tokens 转发，下游相加会得到约两倍（issue #25）。
//
// CC 已经算好非缓存部分（noCacheTokens + cacheReadTokens == inputTokens），优先采用；
// 字段缺失时用减法兜底，保证老版本上游也正确。
func anthropicInputTokens(u *ccUsage, noCacheOverride int64) int64 {
	if noCacheOverride >= 0 {
		return noCacheOverride
	}
	if u == nil {
		return 0
	}
	if d := u.InputTokenDetails; d != nil && d.NoCacheTokens != nil && *d.NoCacheTokens >= 0 {
		return *d.NoCacheTokens
	}
	v := u.InputTokens - u.cacheRead() - u.cacheWrite()
	if v < 0 {
		return 0
	}
	return v
}

func mapFinishReason(reason string) string {
	switch reason {
	case "tool-calls":
		return "tool_calls"
	case "length", "stop":
		return reason
	case "":
		return "stop"
	default:
		return reason
	}
}

func mapAnthropicStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// ── 上游错误映射 ────────────────────────────────────

type mappedError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter int
}

var ccStatusMap = map[int]struct {
	Status int
	Type   string
}{
	400: {400, "invalid_request_error"},
	401: {401, "authentication_error"},
	402: {429, "rate_limit_error"}, // payment required → 限流，让 SDK 退避重试
	403: {401, "authentication_error"},
	404: {404, "not_found"},
	422: {400, "invalid_request_error"},
	429: {429, "rate_limit_error"},
	500: {502, "upstream_error"},
	502: {502, "upstream_error"},
	503: {503, "temporarily_unavailable"},
}

func mapCCStatus(status int, body []byte) mappedError {
	mapped, ok := ccStatusMap[status]
	if !ok {
		mapped = struct {
			Status int
			Type   string
		}{502, "upstream_error"}
	}
	msg := "CC API error (" + strconv.Itoa(status) + ")"
	if len(body) > 0 {
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			switch {
			case parsed.Error.Message != "":
				msg = parsed.Error.Message
			case parsed.Message != "":
				msg = parsed.Message
			}
		} else {
			trimmed := bytes.TrimSpace(body)
			if len(trimmed) > 200 {
				trimmed = trimmed[:200]
			}
			if len(trimmed) > 0 {
				msg = string(trimmed)
			}
		}
	}
	out := mappedError{Status: mapped.Status, Type: mapped.Type, Message: msg}
	if mapped.Status == 429 {
		out.RetryAfter = 30
	}
	return out
}

func mapCCEventError(ev *ccEvent) mappedError {
	msg := ev.errorMessage()
	status := 502
	if len(msg) > 4 && msg[0] == '<' {
		if close := bytes.IndexByte([]byte(msg), '>'); close > 1 && close <= 4 {
			if n, err := strconv.Atoi(msg[1:close]); err == nil {
				status = n
			}
		}
	}
	mapped, ok := ccStatusMap[status]
	if !ok {
		mapped = struct {
			Status int
			Type   string
		}{502, "upstream_error"}
	}
	out := mappedError{Status: mapped.Status, Type: mapped.Type, Message: msg}
	if mapped.Status == 429 {
		out.RetryAfter = 30
	}
	return out
}

// ── Anthropic thinking 签名 ──────────────────────────

// fakeThinkingSignature 生成 Claude 格式的 thinking 签名。
// Anthropic 官方签名是密码学签名的，第三方代理无法伪造；Claude Code 只做浅层校验：
// base64、首字符 'E'（单层）/ 'R'（双层）、payload 首字节 0x12。载荷由 thinking 文本
// 派生，保证每个块的签名不同（更接近规范，避免相同签名的怪问题）。
func fakeThinkingSignature(thinkingText string) string {
	if thinkingText == "" {
		thinkingText = "dsh-proxy-thinking"
	}
	sum := sha256.Sum256([]byte(thinkingText))
	seed := sum[:32]
	raw := make([]byte, 0, len(seed)+2)
	raw = append(raw, 0x12, byte(len(seed)))
	raw = append(raw, seed...)
	return base64StdEncode(raw)
}

// ── 其它工具 ────────────────────────────────────────

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func unixNow() int64 { return time.Now().Unix() }

// toolArgsJSON 取出 tool-call 参数的 JSON 文本：
// 上游的 input 既可能是对象，也可能是"内容是 JSON 的字符串"。
func toolArgsJSON(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("{}")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			if bytes.TrimSpace([]byte(s)) == nil || len(bytes.TrimSpace([]byte(s))) == 0 {
				return []byte("{}")
			}
			return []byte(s)
		}
	}
	return trimmed
}

func rawOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
