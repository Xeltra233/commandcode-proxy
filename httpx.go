package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── 请求体读取 ──────────────────────────────────────
//
// 旧实现要把 body 在内存里复制 5~7 份（chunks/concat/字符串/对象树/重建对象树/序列化），
// 100MB 上限意味着单请求最坏 ~550MB。Go 版本只保留两份：原始字节 + 解析后的结构体
// （解析后原始字节立刻归还池），最坏约 2.2x。
var bodyPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64<<10)
		return &b
	},
}

func getBodyBuf() *[]byte  { return bodyPool.Get().(*[]byte) }
func putBodyBuf(b *[]byte) { *b = (*b)[:0]; bodyPool.Put(b) }

// errBodyTooLarge 对应 413；http.MaxBytesReader 会顺带要求关闭连接，
// 避免客户端继续往一个已拒绝的请求里灌数据。
var errBodyTooLarge = errors.New("request body too large")

// readBody 读取请求体：返回池化缓冲 + 释放函数（调用方解析完必须 release）。
// 不额外复制一份，全程只有"原始字节 + 解析后的对象"两份内存。
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, func(), error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	bufp := getBodyBuf()
	buf := (*bufp)[:0]
	release := func() {
		*bufp = buf[:0]
		putBodyBuf(bufp)
	}
	for {
		if cap(buf)-len(buf) < 32<<10 {
			buf = append(buf, make([]byte, 32<<10)...)
			buf = buf[:len(buf)-32<<10]
		}
		n, err := r.Body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				release()
				return nil, nil, errBodyTooLarge
			}
			release()
			return nil, nil, err
		}
	}
	if len(buf) > 0 && buf[0] == 0xEF { // 去 BOM
		buf = bytes.TrimPrefix(buf, []byte{0xEF, 0xBB, 0xBF})
	}
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		release()
		return nil, nil, errors.New("empty body")
	}
	return buf, release, nil
}

// ── 响应写出 ────────────────────────────────────────

type errBody struct {
	Error      errDetail `json:"error"`
	RetryAfter int       `json:"retry_after,omitempty"`
}

type errDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeJSON(w http.ResponseWriter, status int, v any, retryAfter int) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"encode error","type":"internal_error"}}`))
		return
	}
	b := buf.Bytes()
	if b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string, retryAfter int) {
	writeJSON(w, status, errBody{Error: errDetail{Message: msg, Type: typ}, RetryAfter: retryAfter}, retryAfter)
}

type anthropicErrBody struct {
	Type       string    `json:"type"`
	Error      errDetail `json:"error"`
	RetryAfter int       `json:"retry_after,omitempty"`
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string, retryAfter int) {
	h := w.Header()
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeJSON(w, status, anthropicErrBody{Type: "error", Error: errDetail{Message: msg, Type: typ}, RetryAfter: retryAfter}, retryAfter)
}

type responsesErrDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Param   any    `json:"param"`
}

type responsesErrBody struct {
	Error      responsesErrDetail `json:"error"`
	RetryAfter int                `json:"retry_after,omitempty"`
}

func writeResponsesError(w http.ResponseWriter, status int, typ, msg string, retryAfter int) {
	writeJSON(w, status, responsesErrBody{
		Error:      responsesErrDetail{Message: msg, Type: typ},
		RetryAfter: retryAfter,
	}, retryAfter)
}

// ── SSE 写出 ────────────────────────────────────────
//
// 设计目标：
//  1. 首帧之前只做内存暂存 —— 上游零输出时还能改发 JSON 错误（让 SDK 自动重试），
//     与旧实现"延迟写 200 header"的语义一致。
//  2. 一旦开始下发就用 bufio 批量写（32KB），每个读批次 flush 一次：
//     不在每个 delta 上做一次系统调用，也不额外增加延迟。
//  3. 下游僵死保护：可选写超时（等价于旧实现的 client drain timeout）。
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	stall   time.Duration
	bw      *bufio.Writer
	pending []byte
	started bool
	failed  bool
}

func newSSEWriter(w http.ResponseWriter, stall time.Duration) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w), stall: stall}
}

var sseHeader = map[string]string{
	"Content-Type":      "text/event-stream",
	"Cache-Control":     "no-cache",
	"Connection":        "keep-alive",
	"X-Accel-Buffering": "no",
}

func (s *sseWriter) Started() bool { return s.started }

// Start 下发 200 + SSE 头，并把首帧之前暂存的内容一起写出。
func (s *sseWriter) Start() error {
	if s.started || s.failed {
		return nil
	}
	h := s.w.Header()
	for k, v := range sseHeader {
		h.Set(k, v)
	}
	s.w.WriteHeader(http.StatusOK)
	s.started = true
	s.bw = bufio.NewWriterSize(s.w, 32<<10)
	if len(s.pending) > 0 {
		if _, err := s.bw.Write(s.pending); err != nil {
			s.failed = true
			return err
		}
		s.pending = s.pending[:0]
	}
	return nil
}

// Write 追加一帧（调用方可以立刻回收该帧的内存：未 Start 时会被复制）。
func (s *sseWriter) Write(frame []byte) error {
	if s.failed {
		return errClientGone
	}
	if !s.started {
		s.pending = append(s.pending, frame...)
		return nil
	}
	if _, err := s.bw.Write(frame); err != nil {
		s.failed = true
		return err
	}
	return nil
}

// Flush 把缓冲区推给下游。每个上游读批次调用一次即可。
func (s *sseWriter) Flush() error {
	if !s.started || s.failed {
		return nil
	}
	if s.stall > 0 {
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.stall))
	}
	err := s.bw.Flush()
	if s.stall > 0 {
		_ = s.rc.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		s.failed = true
		return err
	}
	return nil
}

// Discard 丢弃暂存帧（用于改发 JSON 错误）。
func (s *sseWriter) Discard() {
	if !s.started {
		s.pending = s.pending[:0]
	}
}

var errClientGone = errors.New("client connection closed")

// ── 在途请求上限 ────────────────────────────────────

type inflightLimiter struct {
	max int64
	cur atomic.Int64
}

func newInflightLimiter(max int64) *inflightLimiter {
	return &inflightLimiter{max: max}
}

func (l *inflightLimiter) Current() int64 { return l.cur.Load() }

// Acquire 返回 false 表示超过上限；调用方必须回 503 + Retry-After。
func (l *inflightLimiter) Acquire() bool {
	if l == nil || l.max <= 0 {
		return true
	}
	if l.cur.Add(1) > l.max {
		l.cur.Add(-1)
		return false
	}
	return true
}

func (l *inflightLimiter) Release() {
	if l == nil || l.max <= 0 {
		return
	}
	if l.cur.Add(-1) < 0 {
		l.cur.Store(0)
	}
}

// ── 常用头解析 ──────────────────────────────────────

// sessionFromHeaders 复用客户端携带的会话标识（prompt cache 亲和性依赖它稳定）。
func sessionFromHeaders(r *http.Request, promptCacheKey string) string {
	if promptCacheKey != "" && len(promptCacheKey) >= 8 {
		return promptCacheKey
	}
	for _, name := range []string{"x-session-id", "x-claude-code-session-id", "session_id"} {
		if v := r.Header.Get(name); v != "" && len(v) >= 8 {
			return v
		}
	}
	return ""
}

func wantsZDR(r *http.Request, cfgZDR bool) bool {
	return cfgZDR || r.Header.Get("x-cmd-zdr") == "1"
}

func trimmedLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
