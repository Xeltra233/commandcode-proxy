package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// proxyServer 把三个协议端点的公共部分（鉴权、上游调用、错误映射）收在一起。
// 全部字段在启动后只读，请求路径上无锁；唯一可变的是 consecutiveTimeouts（atomic）。
type proxyServer struct {
	cfg     *Config
	log     *logger
	cc      *ccClient
	keys    *keyPool
	auth    *authenticator
	limiter *inflightLimiter

	// blockedModels：运行时自适应——上游以套餐原因拒绝的模型自动从
	// 列表剔除（plan.go）
	blockedModels *modelBlocklist

	consecutiveTimeouts atomic.Int64
}

// retryableStatus 只有"上游在生成前就拒绝"的状态码才换 key 重试：
// 鉴权失败、限流、上游故障。400/404/422 属于请求本身的问题，换 key 也一样失败。
var retryableStatus = map[int]bool{
	401: true, 402: true, 403: true, 429: true,
	500: true, 502: true, 503: true, 504: true,
}

// generateWithFailover 用选定的 key 调上游；命中可重试状态码时把该 key 打入冷却，
// 换池里另一把再试（最多 KeyFailover 次）。传输层错误不重试 —— 请求可能已经到达
// 上游并产生了费用，重试会造成重复生成。
func (s *proxyServer) generateWithFailover(
	ctx context.Context,
	ar authResult,
	body []byte,
	sessionID, slug string,
	stream, zdr bool,
) (*http.Response, *UpstreamKey, error) {
	attempts := 1 + s.cfg.KeyFailover
	if s.keys.len() <= 1 || ar.byo || (ar.client != nil && ar.client.upstream != nil) {
		attempts = 1
	}
	var excluded []bool
	if s.keys.len() > 0 {
		excluded = make([]bool, s.keys.len())
	}
	key := ar.upstream
	for attempt := 0; attempt < attempts; attempt++ {
		if key == nil {
			key = s.keys.next(excluded)
			if key == nil {
				break
			}
		}
		if key.Index >= 0 && key.Index < len(excluded) {
			excluded[key.Index] = true
		}
		s.cc.ensureInitialized(ctx, key)
		resp, err := s.cc.generate(ctx, key, body, sessionID, slug, stream, zdr)
		if err != nil {
			return nil, key, err
		}
		if resp.StatusCode/100 == 2 {
			key.markHealthy()
			return resp, key, nil
		}
		if !retryableStatus[resp.StatusCode] || attempt == attempts-1 {
			return resp, key, nil
		}
		status := resp.StatusCode
		drainAndClose(resp.Body)
		key.cooldown(status, s.cfg.KeyCooldown)
		s.log.Warn("Upstream rejected request, failing over to next key",
			"status", status, "key", key.label(), "attempt", attempt+1, "maxAttempts", attempts)
		key = nil
	}
	return nil, key, errNoUpstreamKey
}

func readErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	defer drainAndClose(resp.Body)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return b
}

// writeMappedError 按协议输出错误：三个端点的 JSON 形状不同，但状态码/类型/文案一致。
func (s *proxyServer) writeMappedError(w http.ResponseWriter, protocol string, e mappedError) {
	switch protocol {
	case "anthropic":
		writeAnthropicError(w, e.Status, e.Type, e.Message, e.RetryAfter)
	case "responses":
		writeResponsesError(w, e.Status, e.Type, e.Message, e.RetryAfter)
	default:
		writeOpenAIError(w, e.Status, e.Type, e.Message, e.RetryAfter)
	}
}

// authFailure 把鉴权错误映射成 HTTP 语义。
func authFailure(err error) mappedError {
	switch {
	case errors.Is(err, errMissingKey):
		return mappedError{Status: 401, Type: "authentication_error",
			Message: "Missing API key. Send in Authorization: Bearer <key> or x-api-key header"}
	case errors.Is(err, errInvalidKey):
		return mappedError{Status: 401, Type: "authentication_error", Message: "Invalid API key"}
	case errors.Is(err, errNoUpstreamKey):
		return mappedError{Status: 503, Type: "configuration_error",
			Message: "No upstream API key configured on this proxy (set CC_API_KEYS or apiKeys in config.json)"}
	default:
		return mappedError{Status: 401, Type: "authentication_error", Message: err.Error()}
	}
}

// authorize 完成下游鉴权 + 上游 key 选择。
func (s *proxyServer) authorize(r *http.Request) (authResult, error) {
	return s.auth.resolve(r, s.keys)
}

// timeoutMessage 连续多次超时后提示压缩上下文（旧实现同样行为）。
func (s *proxyServer) timeoutMessage() string {
	if s.consecutiveTimeouts.Load() >= 3 {
		return "Response timeout - try reducing context length (summarize earlier messages)"
	}
	return "Response timeout - request timed out"
}

func (s *proxyServer) noteTimeout() int64 { return s.consecutiveTimeouts.Add(1) }
func (s *proxyServer) noteSuccess()       { s.consecutiveTimeouts.Store(0) }

// upstreamCancelled 判断错误是否来自客户端断连（此时不该再写响应）。
func upstreamCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func sinceMs(t time.Time) int64 { return time.Since(t).Milliseconds() }
