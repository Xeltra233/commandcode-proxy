package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── 套餐自动检测 ────────────────────────────────────
//
// 与 CLI 的 /usage 面板同一条链路（fetchUsageData）：
//
//	GET /alpha/whoami?limits=1                    → org.id
//	GET /alpha/billing/credits?orgId=<id>         → credits.{planId, purchasedCredits, freeCredits}
//	GET /alpha/billing/subscriptions?orgId=<id>   → data.{planId, status}
//
// 鉴权就是普通上游 key（Authorization: Bearer user_xxx）。拿到 planId
// 后按 CLI 内置的 evaluateModelAccess 逻辑评估每个模型是否可用。

const (
	planDetectTTL     = 30 * time.Minute // 成功探测的缓存时长
	planDetectFailTTL = 2 * time.Minute  // 失败结果的缓存时长（避免每请求重试打爆上游）
	planDetectTimeout = 8 * time.Second
)

// planAccess 一次探测得到的账号套餐状态。
type planAccess struct {
	PlanID           string
	PurchasedCredits float64
	FreeCredits      float64
}

// planDetector 按上游 key 缓存探测结果。
type planDetector struct {
	mu sync.Mutex
	m  map[string]*planDetectEntry
}

type planDetectEntry struct {
	access  *planAccess // nil = 探测失败（按 expires 重试）
	expires time.Time
}

func newPlanDetector() *planDetector {
	return &planDetector{m: make(map[string]*planDetectEntry)}
}

// get 返回缓存的探测结果；过期或未命中返回 nil,false。
func (d *planDetector) get(keyValue string) (*planAccess, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.m[keyValue]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.access, true
}

func (d *planDetector) put(keyValue string, access *planAccess, ttl time.Duration) {
	d.mu.Lock()
	d.m[keyValue] = &planDetectEntry{access: access, expires: time.Now().Add(ttl)}
	d.mu.Unlock()
}

// planAccessFor 返回 key 的套餐状态；bypass=true 表示无需再查
// （purchasedCredits>0 时 CLI 视为全解锁）。
func (c *ccClient) planAccessFor(ctx context.Context, key *UpstreamKey) (*planAccess, bool) {
	if c.planDetector == nil {
		return nil, false
	}
	if pa, ok := c.planDetector.get(key.Value); ok {
		return pa, pa != nil && pa.unlimited()
	}
	pa, err := c.fetchPlanAccess(ctx, key)
	if err != nil {
		c.log.Warn("Plan detection failed, model list NOT plan-filtered (fail-open); check key/network or set CC_PLAN", "key", key.label(), "error", err.Error())
		c.planDetector.put(key.Value, nil, planDetectFailTTL)
		return nil, false
	}
	c.log.Info("Plan detected",
		"key", key.label(), "planId", pa.PlanID,
		"purchasedCredits", pa.PurchasedCredits, "freeCredits", pa.FreeCredits)
	c.planDetector.put(key.Value, pa, planDetectTTL)
	return pa, pa.unlimited()
}

func (pa *planAccess) unlimited() bool {
	return pa.PurchasedCredits > 0 || pa.FreeCredits > 0
}

type whoamiResponse struct {
	Org struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	} `json:"org"`
	User struct {
		UserName string `json:"userName"`
	} `json:"user"`
}

type creditsResponse struct {
	Credits struct {
		PlanID           string  `json:"planId"`
		MonthlyCredits   float64 `json:"monthlyCredits"`
		PurchasedCredits float64 `json:"purchasedCredits"`
		FreeCredits      float64 `json:"freeCredits"`
	} `json:"credits"`
}

type subscriptionResponse struct {
	Data struct {
		PlanID string `json:"planId"`
		Status string `json:"status"`
	} `json:"data"`
}

// fetchPlanAccess 执行 whoami → credits + subscriptions 探测。
func (c *ccClient) fetchPlanAccess(ctx context.Context, key *UpstreamKey) (*planAccess, error) {
	detectCtx, cancel := context.WithTimeout(ctx, planDetectTimeout)
	defer cancel()

	get := func(endpoint string) ([]byte, error) {
		req, err := http.NewRequestWithContext(detectCtx, http.MethodGet, c.baseURL+endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key.Value)
		req.Header.Set("x-cli-environment", "production")
		req.Header.Set("x-command-code-version", c.versionString())
		resp, err := c.short.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return nil, &statusError{status: resp.StatusCode, endpoint: endpoint, body: string(body)}
		}
		return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}

	whoamiBody, err := get("/alpha/whoami?limits=1")
	if err != nil {
		return nil, err
	}
	var whoami whoamiResponse
	_ = json.Unmarshal(whoamiBody, &whoami)
	orgID := whoami.Org.ID

	pa := &planAccess{}
	// credits 与 subscriptions 并行（与 CLI fetchUsageData 一致）；任一成功即可
	// 个人账号 whoami 返回 org:null —— 此时与 CLI 一致，省略 orgId 参数
	billingQuery := ""
	if orgID != "" {
		billingQuery = "?orgId=" + orgID
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		body, err := get("/alpha/billing/credits" + billingQuery)
		if err != nil {
			mu.Lock()
			firstErr = err
			mu.Unlock()
			return
		}
		var cr creditsResponse
		if json.Unmarshal(body, &cr) == nil && cr.Credits.PlanID != "" {
			mu.Lock()
			pa.PlanID = cr.Credits.PlanID
			pa.PurchasedCredits = cr.Credits.PurchasedCredits
			pa.FreeCredits = cr.Credits.FreeCredits
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		body, err := get("/alpha/billing/subscriptions" + billingQuery)
		if err != nil {
			mu.Lock()
			firstErr = err
			mu.Unlock()
			return
		}
		var sr subscriptionResponse
		if json.Unmarshal(body, &sr) == nil && sr.Data.PlanID != "" {
			mu.Lock()
			if pa.PlanID == "" {
				pa.PlanID = sr.Data.PlanID
			}
			mu.Unlock()
		}
	}()
	wg.Wait()
	if pa.PlanID == "" {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errPlanUnknown
	}
	return pa, nil
}

type statusError struct {
	status   int
	endpoint string
	body     string
}

func (e *statusError) Error() string {
	return e.endpoint + ": status " + itoa(e.status)
}

var errPlanUnknown = errors.New("planId not found in billing endpoints")

// ── 访问评估（复刻 CLI evaluateModelAccess） ────────

// canonicalizeModelID 复刻 CLI canonicalizeModelId：
// 小写 → 别名表归一 → 去掉 "-YYYYMMDD"/"@YYYYMMDD" 日期后缀再归一。
func canonicalizeModelID(model string) string {
	if model == "" {
		return model
	}
	resolve := func(s string) string {
		lower := strings.ToLower(s)
		if alias, ok := modelAliasTable[lower]; ok {
			return alias
		}
		return lower
	}
	if c := resolve(model); c != model || modelAliasTable[strings.ToLower(model)] != "" {
		if _, known := modelMetaTable[c]; known {
			return c
		}
	}
	lower := resolve(model)
	if _, known := modelMetaTable[lower]; known {
		return lower
	}
	// 去日期后缀再试
	stripped := stripDateSuffix(lower)
	if stripped != lower {
		if alias, ok := modelAliasTable[stripped]; ok {
			stripped = alias
		}
		if _, known := modelMetaTable[stripped]; known {
			return stripped
		}
	}
	return lower
}

var dateSuffixRe = regexp.MustCompile(`[-@]\d{8}$`)

func stripDateSuffix(id string) string {
	if loc := dateSuffixRe.FindStringIndex(id); loc != nil {
		return id[:loc[0]]
	}
	return id
}

// evaluateModelAccess 判断当前套餐能否使用该模型（返回 false 表示不可用）。
// 逻辑与 CLI evaluateModelAccess 逐条对应：
//  1. purchased/free credits > 0 → 全解锁
//  2. 无 planId / 未知 planId → 不过滤
//  3. 模型不在官方元数据表 → 不过滤（保守放行未知模型，由运行时
//     拒绝拉黑兜底）
//  4. category 在套餐允许列表内且不在黑名单 → 允许
func evaluateModelAccess(model string, pa *planAccess) bool {
	if pa == nil || pa.unlimited() || pa.PlanID == "" {
		return true
	}
	rule, ok := planAccessRules[pa.PlanID]
	if !ok {
		return true
	}
	canonical := canonicalizeModelID(model)
	meta, known := modelMetaTable[canonical]
	if !known {
		return true
	}
	full := meta.Provider + ":" + canonical
	for _, b := range rule.BlockedModels {
		if b == full {
			return false
		}
	}
	for _, cat := range rule.AllowedCategories {
		if cat == meta.Category {
			return true
		}
	}
	return false
}

// filterModelsByPlanAccess 按探测到的套餐状态过滤模型列表。
func filterModelsByPlanAccess(models []modelInfo, pa *planAccess, blocked *modelBlocklist) []modelInfo {
	if pa == nil || pa.unlimited() {
		if blocked == nil || blocked.len() == 0 {
			return models
		}
	}
	out := models[:0:0]
	for _, m := range models {
		if blocked != nil && blocked.blocked(m.ID) {
			continue
		}
		if pa != nil && !pa.unlimited() && !evaluateModelAccess(m.ID, pa) {
			continue
		}
		out = append(out, m)
	}
	return out
}
