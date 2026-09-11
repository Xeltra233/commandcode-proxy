package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 上游 key 的健康度：命中 401/402/403/429 或上游 5xx 时冷却一段时间，
// 让轮询先把流量分给健康 key；冷却结束后自动重新参与。
const (
	sessionDuration    = 12 * time.Hour
	sessionJitter      = 1 * time.Hour
	initRefresh        = 8 * time.Hour
	initJitter         = 2 * time.Hour
	defaultKeyCooldown = 60 * time.Second
)

var (
	errMissingKey    = errors.New("missing API key")
	errInvalidKey    = errors.New("invalid API key")
	errNoUpstreamKey = errors.New("no upstream API key configured on this proxy")
)

// UpstreamKey 是一把 Command Code 上游 key 及其附属状态。
// 每个 key 拥有独立的 session 与设备指纹（旧版实现的 keyStateStore 语义），
// 保证 prompt cache 亲和性不会被别的 key 打散。
type UpstreamKey struct {
	Value string
	Name  string
	Index int

	mu             sync.Mutex
	sessionID      string
	sessionExpires time.Time
	nextInitAt     time.Time
	fp             *fingerprint

	unhealthyUntil atomic.Int64 // unix nano，0 = 健康
	requests       atomic.Int64
	failures       atomic.Int64
}

// label 只暴露 key 前缀：日志里永远不出现完整 key（敏感信息红线）。
func (k *UpstreamKey) label() string {
	if k == nil {
		return "(none)"
	}
	prefix := k.Value
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	if k.Name != "" {
		return k.Name + "/" + prefix + "…"
	}
	return prefix + "…"
}

// sessionIDFor 返回该 key 的 CC session：12h + 0~1h 抖动，到期自动换新。
func (k *UpstreamKey) sessionIDFor(now time.Time) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.sessionID != "" && now.Before(k.sessionExpires) {
		return k.sessionID
	}
	k.sessionID = newUUID()
	k.sessionExpires = now.Add(sessionDuration + time.Duration(randInt63n(int64(sessionJitter))))
	return k.sessionID
}

// fingerprintForInit 返回指纹并在需要时放行一次初始化预请求（首次 + 每 8h±2h）。
func (k *UpstreamKey) fingerprintForInit(now time.Time) (*fingerprint, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fp == nil {
		k.fp = newFingerprint()
	}
	if now.Before(k.nextInitAt) {
		return k.fp, false
	}
	k.nextInitAt = now.Add(initRefresh + time.Duration(randInt63n(int64(initJitter))))
	return k.fp, true
}

func (k *UpstreamKey) healthy(now time.Time) bool {
	return k.unhealthyUntil.Load() <= now.UnixNano()
}

// cooldown 在 key 不可用（鉴权失败/限流/上游 5xx）时打冷标记。
func (k *UpstreamKey) cooldown(status int, d time.Duration) {
	if d <= 0 {
		return
	}
	// 鉴权类错误冷却更久：这类 key 短时间内不会自愈
	switch status {
	case 401, 402, 403:
		d *= 5
	}
	until := time.Now().Add(d).UnixNano()
	for {
		cur := k.unhealthyUntil.Load()
		if cur >= until || k.unhealthyUntil.CompareAndSwap(cur, until) {
			break
		}
	}
	k.failures.Add(1)
}

func (k *UpstreamKey) markHealthy() {
	k.unhealthyUntil.Store(0)
}

// ── 上游 key 池 ─────────────────────────────────────

type keyPool struct {
	keys     []*UpstreamKey
	byValue  map[string]*UpstreamKey
	strategy string
	rr       atomic.Uint64
}

func newKeyPool(cfgs []UpstreamKeyConfig, strategy string) *keyPool {
	p := &keyPool{strategy: strategy, byValue: make(map[string]*UpstreamKey, len(cfgs))}
	for i, c := range cfgs {
		k := &UpstreamKey{Value: c.Key, Name: c.Name, Index: i}
		p.keys = append(p.keys, k)
		p.byValue[c.Key] = k
	}
	return p
}

func (p *keyPool) len() int {
	if p == nil {
		return 0
	}
	return len(p.keys)
}

func (p *keyPool) first() *UpstreamKey {
	if p == nil || len(p.keys) == 0 {
		return nil
	}
	return p.keys[0]
}

func (p *keyPool) byIndex(i int) *UpstreamKey {
	if p == nil || i < 0 || i >= len(p.keys) {
		return nil
	}
	return p.keys[i]
}

func (p *keyPool) lookup(value string) *UpstreamKey {
	if p == nil {
		return nil
	}
	return p.byValue[value]
}

// next 轮询/随机挑一把可用 key。
// excluded 标记本轮已经试过的 key（failover 重试时用）；全部不可用时返回 nil。
func (p *keyPool) next(excluded []bool) *UpstreamKey {
	n := p.len()
	if n == 0 {
		return nil
	}
	var start uint64
	if p.strategy == "random" {
		start = randUint64()
	} else {
		start = p.rr.Add(1) - 1
	}
	now := time.Now().UnixNano()
	var unhealthy *UpstreamKey
	for i := 0; i < n; i++ {
		k := p.keys[(start+uint64(i))%uint64(n)]
		if excluded != nil && k.Index < len(excluded) && excluded[k.Index] {
			continue
		}
		if k.unhealthyUntil.Load() <= now {
			return k
		}
		if unhealthy == nil {
			unhealthy = k
		}
	}
	// 全部处于冷却期：仍然返回一把（上游可能已经恢复），由调用方决定是否重试
	return unhealthy
}

// ── 下游 key（分发给客户端使用的 key） ──────────────

type clientKey struct {
	Value    string
	Name     string
	upstream *UpstreamKey // nil = 使用上游池
	requests atomic.Int64
}

func (c *clientKey) label() string {
	if c == nil {
		return "(anonymous)"
	}
	if c.Name != "" {
		return c.Name
	}
	prefix := c.Value
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return prefix + "…"
}

type authenticator struct {
	byValue map[string]*clientKey
	keys    []*clientKey
	enabled bool
	// allowBYO 允许客户端直接带上自己的上游 key（user_xxx）当上游凭据用
	allowBYO bool

	// 自带 key 的客户端按 key 缓存 UpstreamKey：session / 指纹 / 冷却状态需要跨请求保留，
	// 否则每请求换一个 session 会打散 prompt cache 亲和性。
	byoMu   sync.Mutex
	byoKeys map[string]*byoEntry
}

type byoEntry struct {
	key      *UpstreamKey
	lastSeen time.Time
}

func newAuthenticator(cfgs []ClientKeyConfig, pool *keyPool, allowBYO bool) (*authenticator, []string) {
	a := &authenticator{
		byValue:  make(map[string]*clientKey, len(cfgs)),
		enabled:  len(cfgs) > 0,
		allowBYO: allowBYO,
		byoKeys:  make(map[string]*byoEntry),
	}
	var warnings []string
	for _, c := range cfgs {
		ck := &clientKey{Value: c.Key, Name: c.Name}
		switch {
		case c.UpstreamKey != "":
			ck.upstream = pool.lookup(c.UpstreamKey)
			if ck.upstream == nil {
				warnings = append(warnings, fmt.Sprintf(
					"clientKeys 里的 %s 指定了不存在的 upstreamKey（该下游 key 将回落到上游池）", ck.label()))
			}
		case c.UpstreamIndex != nil:
			ck.upstream = pool.byIndex(*c.UpstreamIndex)
			if ck.upstream == nil {
				warnings = append(warnings, fmt.Sprintf(
					"clientKeys 里的 %s 指定了越界的 upstreamKeyIndex=%d（该下游 key 将回落到上游池）",
					ck.label(), *c.UpstreamIndex))
			}
		}
		a.byValue[ck.Value] = ck
		a.keys = append(a.keys, ck)
	}
	return a, warnings
}

type authResult struct {
	upstream  *UpstreamKey
	client    *clientKey
	presented string
	byo       bool
}

// byoKey 返回（并缓存）客户端自带 key 对应的 UpstreamKey。
func (a *authenticator) byoKey(value string) *UpstreamKey {
	a.byoMu.Lock()
	defer a.byoMu.Unlock()
	if e, ok := a.byoKeys[value]; ok {
		e.lastSeen = time.Now()
		return e.key
	}
	k := &UpstreamKey{Value: value, Name: "byo", Index: -1}
	a.byoKeys[value] = &byoEntry{key: k, lastSeen: time.Now()}
	return k
}

// janitor 定期清理长期不用的自带 key 状态（默认 1h，与旧实现的 session 清理一致）。
func (a *authenticator) janitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-sessionDuration)
			a.byoMu.Lock()
			for k, e := range a.byoKeys {
				if e.lastSeen.Before(cutoff) {
					delete(a.byoKeys, k)
				}
			}
			a.byoMu.Unlock()
		}
	}
}

func looksLikeUpstreamKey(s string) bool {
	return strings.HasPrefix(s, "user_") && len(s) > len("user_")
}

// extractPresentedKey 从 OpenAI 风格 Authorization: Bearer 或 Anthropic 风格 x-api-key 取 key。
func extractPresentedKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := cutPrefixFold(auth, "bearer "); ok {
			if k := extractUserKey(after); k != "" {
				return k
			}
			return strings.TrimSpace(after)
		}
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		if got := extractUserKey(k); got != "" {
			return got
		}
		return strings.TrimSpace(k)
	}
	// Gemini 风格：x-goog-api-key 头或 ?key= 查询参数
	if k := r.Header.Get("x-goog-api-key"); k != "" {
		if got := extractUserKey(k); got != "" {
			return got
		}
		return strings.TrimSpace(k)
	}
	if k := r.URL.Query().Get("key"); k != "" {
		if got := extractUserKey(k); got != "" {
			return got
		}
		return strings.TrimSpace(k)
	}
	return ""
}

// extractUserKey 兼容 "Bearer user_xxx" 里夹带其它内容（有的客户端会带上额外字段）。
func extractUserKey(s string) string {
	idx := strings.Index(s, "user_")
	if idx < 0 {
		return ""
	}
	end := idx + len("user_")
	for end < len(s) {
		c := s[end]
		if c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			end++
			continue
		}
		break
	}
	return s[idx:end]
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return s, false
	}
	if strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// resolve 完成下游鉴权 + 上游 key 选择。
//
// 规则（由宽到严，取决于是否配置了 clientKeys）：
//  1. 未配置 clientKeys：透传模式。请求头里的上游 key 直接用；没有则用上游池第一把。
//     这是旧版本的行为，个人部署零配置可用。
//  2. 配置了 clientKeys：请求头里的 key 必须命中 clientKeys，否则 401。
//     上游 key 取该下游 key 绑定的那一把，否则走上游池轮询。
//  3. allowBYO=true 时，任何用户都可以带自己的 user_xxx 当上游凭据（不校验分发 key）。
func (a *authenticator) resolve(r *http.Request, pool *keyPool) (authResult, error) {
	presented := extractPresentedKey(r)
	res := authResult{presented: presented}

	if a != nil && a.enabled {
		if presented == "" {
			return res, errMissingKey
		}
		if ck, ok := a.byValue[presented]; ok {
			res.client = ck
			ck.requests.Add(1)
			if ck.upstream != nil {
				res.upstream = ck.upstream
				return res, nil
			}
			if k := pool.next(nil); k != nil {
				res.upstream = k
				return res, nil
			}
			return res, errNoUpstreamKey
		}
		if a.allowBYO && looksLikeUpstreamKey(presented) {
			res.byo = true
			res.upstream = pool.lookup(presented)
			if res.upstream == nil {
				res.upstream = a.byoKey(presented)
			}
			return res, nil
		}
		return res, errInvalidKey
	}

	// 透传模式
	if looksLikeUpstreamKey(presented) {
		res.byo = true
		res.upstream = pool.lookup(presented)
		if res.upstream == nil {
			res.upstream = a.byoKey(presented)
		}
		return res, nil
	}
	if k := pool.next(nil); k != nil {
		res.upstream = k
		return res, nil
	}
	if presented == "" {
		return res, errMissingKey
	}
	return res, errNoUpstreamKey
}

// ── 小工具 ──────────────────────────────────────────

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下退化到时间戳，保证不 panic（UUID 只用于会话标记）
		return fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().Unix(), time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}

// randInt63n 返回 [0,n) 的随机数；n<=0 时返回 0。
func randInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(randUint64() % uint64(n))
}

func randHex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n*2)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = digits[randInt63n(16)]
		}
		return string(b)
	}
	for i := range b {
		b[i] = digits[b[i]&0x0f]
	}
	return string(b)
}
