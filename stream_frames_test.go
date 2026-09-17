package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：流式帧必须始终是合法 JSON。
//
// 背景（2026-09 线上事故）：上游没走正常收尾（error 事件）时，chat 的流内错误帧
// 和 Responses 的收尾帧都多写了一个右花括号：
//
//	{"error":{"message":"...","type":"upstream_error"}}}     ← 多一层 }
//	{"type":"response.completed",...,"usage":{...}}}         ← 多一层 }
//
// 下游症状：new-api 报 "invalid character '}' after top-level value"，
// OpenAI SDK 报 "Unexpected non-whitespace character after JSON at position N"。
// 这里用假 CC 上游走完整 handler 路径，逐帧 json.Unmarshal 校验。

const testDownstreamKey = "sk-test-downstream"

const testStreamErrorEvent = `{"type":"error","message":"Upstream stream ended before terminal chunk"}`

const testStreamFinish = `{"type":"text-delta","text":"hello"}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":2}}
`

// fakeCCUpstream 模拟 Command Code 上游：/alpha/generate 返回给定的 NDJSON 事件流，
// 其余端点（fingerprint/record、lifecycle-events）一律 200，保证 ensureInitialized 不阻塞。
func fakeCCUpstream(t *testing.T, ndjson string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/generate":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, ndjson)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeCCUpstreamStall 模拟"发了首事件后静默"的 CC 上游：用于验证首帧之前的空闲超时。
// 上游连接保持打开（不 EOF），确保触发的是 StreamIdle 而不是干净 EOF。
func fakeCCUpstreamStall(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/generate":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"type":"start"}`+"\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProxyServer(t *testing.T, ccURL string) *proxyServer {
	t.Helper()
	cfg := &Config{
		APIBase:             ccURL,
		UpstreamKeys:        []UpstreamKeyConfig{{Key: "user_test_upstream", Name: "test"}},
		ClientKeys:          []ClientKeyConfig{{Key: testDownstreamKey, Name: "test-client"}},
		KeyStrategy:         "round_robin",
		LogLevel:            "error",
		StreamIdle:          30 * time.Second,
		NonstreamIdle:       60 * time.Second,
		KeyCooldown:         30 * time.Second,
		KeyFailover:         1,
		MaxBodyBytes:        8 << 20,
		MaxIdleConnsPerHost: 8,
	}
	log := newLogger(cfg.LogLevel, "")
	pool := newKeyPool(cfg.UpstreamKeys, cfg.KeyStrategy)
	auth, _ := newAuthenticator(cfg.ClientKeys, pool, false)
	return &proxyServer{
		cfg:           cfg,
		log:           log,
		cc:            newCCClient(cfg, log),
		keys:          pool,
		auth:          auth,
		limiter:       newInflightLimiter(cfg.MaxInflight),
		blockedModels: newModelBlocklist(),
	}
}

func postStream(t *testing.T, ps *proxyServer, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDownstreamKey)
	rec := httptest.NewRecorder()
	ps.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("%s: content-type = %q, want text/event-stream", path, ct)
	}
	return rec
}

// sseFrames 解析 SSE 响应体；任何 data 帧不是合法 JSON 就直接失败并打印原始载荷。
func sseFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				t.Fatalf("SSE data frame is not valid JSON: %v\npayload (%d bytes): %s", err, len(payload), payload)
			}
			frames = append(frames, m)
		}
	}
	return frames
}

// 上游没正常收尾（error 事件）时，chat 的流内错误帧必须是合法 JSON。
func TestChatStreamUpstreamErrorFrameIsValidJSON(t *testing.T) {
	upstream := fakeCCUpstream(t, `{"type":"text-delta","text":"hello"}
`+testStreamErrorEvent+`
`)
	ps := newTestProxyServer(t, upstream.URL)
	rec := postStream(t, ps, "/v1/chat/completions",
		`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	errObj, ok := last["error"].(map[string]any)
	if !ok {
		t.Fatalf("last frame is not an error frame: %v", last)
	}
	if got := errObj["message"]; got != "Upstream stream ended before terminal chunk" {
		t.Errorf("error message = %v", got)
	}
}

// 正常走完时 chat 的所有帧（含 usage 收尾 chunk）必须是合法 JSON。
func TestChatStreamFramesAreValidJSON(t *testing.T) {
	upstream := fakeCCUpstream(t, testStreamFinish)
	ps := newTestProxyServer(t, upstream.URL)
	rec := postStream(t, ps, "/v1/chat/completions",
		`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 2 {
		t.Fatalf("expected text + finish frames, got %d", len(frames))
	}
	last := frames[len(frames)-1]
	if _, ok := last["choices"]; !ok {
		t.Fatalf("last frame lacks choices: %v", last)
	}
}

// Responses 的收尾帧（response.completed）必须是合法 JSON。
func TestResponsesStreamTerminalFrameIsValidJSON(t *testing.T) {
	upstream := fakeCCUpstream(t, testStreamFinish)
	ps := newTestProxyServer(t, upstream.URL)
	rec := postStream(t, ps, "/v1/responses",
		`{"model":"test-model","stream":true,"input":[{"role":"user","content":"hi"}]}`)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last frame type = %v, want response.completed", last["type"])
	}
}

// Responses 上游 error 事件输出 response.failed 帧，同样必须是合法 JSON。
func TestResponsesStreamFailedFrameIsValidJSON(t *testing.T) {
	upstream := fakeCCUpstream(t, `{"type":"text-delta","text":"hello"}
`+testStreamErrorEvent+`
`)
	ps := newTestProxyServer(t, upstream.URL)
	rec := postStream(t, ps, "/v1/responses",
		`{"model":"test-model","stream":true,"input":[{"role":"user","content":"hi"}]}`)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last frame type = %v, want response.failed", last["type"])
	}
}

// Anthropic 协议流的所有帧必须是合法 JSON（回归防护）。
func TestAnthropicStreamFramesAreValidJSON(t *testing.T) {
	upstream := fakeCCUpstream(t, testStreamFinish)
	ps := newTestProxyServer(t, upstream.URL)
	rec := postStream(t, ps, "/v1/messages",
		`{"model":"test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	frames := sseFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	if last["type"] != "message_stop" {
		t.Fatalf("last frame type = %v, want message_stop", last["type"])
	}
}

// 回归（2026-09-17 线上事故）：上游只发了一个 start 事件后静默，首帧之前的空闲超时
// 必须回 JSON 错误，让 new-api / SDK 依据状态码与 Retry-After 识别并重试。
// 旧行为：idleErrorFrame 在未 Start 时直接返回错误、handler 零输出退出，
// net/http 默认 200 + 空流 → new-api 记 end_reason=eof/ok，
// pi 报 "Stream ended without finish_reason"（glm-5.3-flash，线上多次复现）。
func TestChatStreamIdleBeforeFirstByteReturnsJSONError(t *testing.T) {
	upstream := fakeCCUpstreamStall(t)
	ps := newTestProxyServer(t, upstream.URL)
	// idleFor 在请求时读取 cfg.StreamIdle，构造后改小即可生效
	ps.cfg.StreamIdle = 200 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDownstreamKey)
	rec := httptest.NewRecorder()
	ps.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("idle timeout before first byte returned 200 (empty stream); want JSON error, body=%q", rec.Body.String())
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("error body is not valid JSON: %v; body=%q", err, rec.Body.String())
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok || errObj["message"] == "" {
		t.Fatalf("unexpected error body shape: %v", m)
	}
}
