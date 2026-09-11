package main

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── 套餐与模型可用性 ────────────────────────────────
//
// 上游 /provider/v1/models 返回全量模型目录，与订阅套餐无关；而账号
// 套餐按官方 CLI 文档明确"无法 headless 读取"（auth token 与接口探测
// 都不含该信息）。因此过滤由两部分组成：
//
//  1. 静态官方表：CC_PLAN 配置一次套餐等级（go/goat/pro/max），按
//     command-code CLI 内置模型目录的 Min plan 列过滤（plan_models.go）。
//  2. 运行时自适应：上游以套餐/额度原因拒绝某模型时，自动把该模型从
//     列表剔除一段时间，到期自动恢复——官方表过期或未配置 CC_PLAN 时，
//     列表也会逐步收敛到真实可用集合。

var planTierRank = map[string]int{"": -1, "go": 0, "goat": 1, "pro": 2, "max": 3}

// normalizePlan 归一化套餐名；空 / "all" 表示不过滤。
func normalizePlan(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "all", "unlimited":
		return ""
	case "go":
		return "go"
	case "goat":
		return "goat"
	case "pro":
		return "pro"
	case "provider", "payg", "pay-as-you-go":
		return "provider" // Provider 按量计费，等同全量
	case "max", "max10", "max10x", "max20", "max20x", "ultra", "team", "teampro":
		return "max"
	default:
		return ""
	}
}

// modelMinTier 查官方目录的最小套餐；second 返回 false 表示目录里没有该模型。
// 先整 id 匹配，再按末段（去掉 provider 前缀）匹配。
func modelMinTier(id string) (int, bool) {
	lower := strings.ToLower(id)
	if t, ok := modelMinPlanTable[lower]; ok {
		return t, true
	}
	if i := strings.LastIndex(lower, "/"); i >= 0 {
		if t, ok := modelMinPlanTable[lower[i+1:]]; ok {
			return t, true
		}
	}
	return 0, false
}

// premiumAllowSet 构造 premium 放行集合：完整 id 与末段 id 都收，
// 这样配置 "claude-sonnet-4-6" 能匹配 "anthropic/claude-sonnet-4-6"。
func premiumAllowSet(allowed []string) map[string]bool {
	set := make(map[string]bool, len(allowed)*2)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		set[a] = true
		if i := strings.LastIndex(a, "/"); i >= 0 {
			set[a[i+1:]] = true
		}
	}
	return set
}

// filterModelsForPlan 按套餐过滤模型列表：
//   - plan 为空 / provider / max：原样返回（官方目录全部可用）
//   - go / goat：只保留 Min plan ≤ 当前套餐的模型，以及 premiumAllow
//     显式放行的模型（对应套餐描述里的 "some premium"——用户可能有
//     per-model allowance 或充值额度）；官方目录没有的新模型保守剔除。
func filterModelsForPlan(models []modelInfo, plan string, premiumAllow []string) []modelInfo {
	rank, ok := planTierRank[plan]
	if !ok || rank < 0 {
		return models
	}
	allow := premiumAllowSet(premiumAllow)
	out := make([]modelInfo, 0, len(models))
	for _, m := range models {
		if allow[strings.ToLower(m.ID)] {
			out = append(out, m)
			continue
		}
		if minTier, known := modelMinTier(m.ID); known && rank >= minTier {
			out = append(out, m)
		}
	}
	return out
}

// ── 运行时自适应：上游拒绝 → 自动剔除 ──────────────

// planBlockRe：上游错误信息里表明"套餐等级不够 / 无该模型额度"的模式。
// 普通限流（rate limit / quota exhausted / credit 不足）不算——那换
// 个模型也一样，不该把模型拉黑。
var planBlockRe = regexp.MustCompile(
	`(?i)(available in (?:go|goat|pro|max|ultra)(?: and above)? plans?|` +
		`not (?:included|available|supported) on your|requires?(?: a)? (?:go|goat|pro|max)(?: plan)?|` +
		`your (?:go|goat|pro|max) plan|plan (?:does not|doesn't) (?:include|support)|` +
		`upgrade (?:your plan|to (?:goat|pro|max))|not part of your plan|no (?:access|allowance) (?:for|to) (?:this )?model)`)

// modelBlocklist 记录被上游以套餐原因拒绝的模型及解除时间。
type modelBlocklist struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newModelBlocklist() *modelBlocklist {
	return &modelBlocklist{m: make(map[string]time.Time)}
}

func (b *modelBlocklist) block(model string, ttl time.Duration) {
	b.mu.Lock()
	b.m[strings.ToLower(model)] = time.Now().Add(ttl)
	b.mu.Unlock()
}

func (b *modelBlocklist) blocked(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	exp, ok := b.m[strings.ToLower(model)]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(b.m, strings.ToLower(model))
		return false
	}
	return true
}

func (b *modelBlocklist) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 顺带清理过期项
	now := time.Now()
	for k, exp := range b.m {
		if now.After(exp) {
			delete(b.m, k)
		}
	}
	return len(b.m)
}

// planBlockTTL：被拒模型的自动剔除时长（到期后重新出现在列表，
// 下次调用若仍被拒会再次剔除——自动收敛，无需人工干预）。
const planBlockTTL = time.Hour

// noteUpstreamModelRejection：上游返回的错误若表明该模型超出当前套餐，
// 把它从模型列表自动剔除。model 为空或无法确认是套餐原因时不动作。
func (s *proxyServer) noteUpstreamModelRejection(model, upstreamMessage string) {
	if model == "" || upstreamMessage == "" || !planBlockRe.MatchString(upstreamMessage) {
		return
	}
	s.blockedModels.block(model, planBlockTTL)
	s.log.Warn("Model auto-excluded from list (plan rejection)",
		"model", model, "retryInMinutes", int(planBlockTTL.Minutes()))
}
