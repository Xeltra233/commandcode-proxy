package main

import (
	"testing"
	"time"
)

func TestNormalizePlan(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"all":    "",
		"go":     "go",
		"GOAT":   "goat",
		"Pro":    "pro",
		"MAX":    "max",
		"max10x": "max",
		"team":   "max",
		"weird":  "",
	}
	for in, want := range cases {
		if got := normalizePlan(in); got != want {
			t.Errorf("normalizePlan(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestModelMinTierOfficialTable(t *testing.T) {
	// 来自官方 models.md Min plan 列
	if tier, ok := modelMinTier("deepseek/deepseek-v4-flash"); !ok || tier != 0 {
		t.Errorf("deepseek-v4-flash min tier = %d,%v; want 0,true", tier, ok)
	}
	if tier, ok := modelMinTier("claude-sonnet-4-6"); !ok || tier != 2 {
		t.Errorf("claude-sonnet-4-6 min tier = %d,%v; want 2,true", tier, ok)
	}
}

func TestFilterModelsForPlanStatic(t *testing.T) {
	models := []modelInfo{
		{ID: "deepseek/deepseek-v4-flash"},
		{ID: "claude-sonnet-4-6"},
	}
	goList := filterModelsForPlan(models, "go", nil)
	if len(goList) != 1 || goList[0].ID != "deepseek/deepseek-v4-flash" {
		t.Errorf("go filter = %v", goList)
	}
	// premium 放行名单
	goList2 := filterModelsForPlan(models, "go", []string{"claude-sonnet-4-6"})
	if len(goList2) != 2 {
		t.Errorf("go filter with allow = %v", goList2)
	}
	// pro 及以上不过滤
	if got := filterModelsForPlan(models, "pro", nil); len(got) != 2 {
		t.Errorf("pro filter = %v", got)
	}
	if got := filterModelsForPlan(models, "", nil); len(got) != 2 {
		t.Errorf("no-plan filter = %v", got)
	}
}

func TestEvaluateModelAccess(t *testing.T) {
	goPlan := &planAccess{PlanID: "individual-go"}
	goatPlan := &planAccess{PlanID: "individual-goat"}
	proPlan := &planAccess{PlanID: "individual-pro"}
	unknown := &planAccess{PlanID: "individual-max"}

	cases := []struct {
		model string
		pa    *planAccess
		want  bool
		note  string
	}{
		{"deepseek/deepseek-v4-flash", goPlan, true, "opensource on go"},
		{"google/gemini-3.5-flash", goPlan, false, "premium blocked on go"},
		{"meta/muse-spark-1.2", goPlan, false, "opensource but on go blacklist"},
		{"xai/grok-4.6", goatPlan, true, "goat has no blacklist"},
		{"google/gemini-3.8-flash", goatPlan, true, "blacklisted only on go"},
		{"claude-sonnet-4-6", proPlan, true, "sonnet on pro"},
		{"claude-opus-4-8", proPlan, false, "opus on pro blacklist"},
		{"claude-opus-4-8", unknown, true, "max allows all"},
		{"brand-new-model-x", goPlan, true, "unknown model passes (runtime blacklist backstop)"},
	}
	for _, c := range cases {
		if got := evaluateModelAccess(c.model, c.pa); got != c.want {
			t.Errorf("evaluateModelAccess(%q, %s) = %v; want %v (%s)",
				c.model, c.pa.PlanID, got, c.want, c.note)
		}
	}
	// 充值/免费额度全解锁
	topUp := &planAccess{PlanID: "individual-go", PurchasedCredits: 5}
	if !evaluateModelAccess("claude-opus-4-8", topUp) {
		t.Error("purchasedCredits should unlock everything")
	}
	free := &planAccess{PlanID: "individual-go", FreeCredits: 1}
	if !evaluateModelAccess("claude-opus-4-8", free) {
		t.Error("freeCredits should unlock everything")
	}
}

func TestCanonicalizeModelID(t *testing.T) {
	if got := canonicalizeModelID("claude-sonnet-4-20250514"); got != "claude-sonnet-4-6" {
		t.Errorf("alias canonicalize = %q", got)
	}
	if got := canonicalizeModelID("claude-opus-4-5-20251101"); got != "claude-opus-4-7" {
		t.Errorf("date-suffix alias = %q", got)
	}
	if got := canonicalizeModelID("DeepSeek/DeepSeek-V4-Flash"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("lowercase canonicalize = %q", got)
	}
}

func TestPlanBlocklist(t *testing.T) {
	b := newModelBlocklist()
	b.block("m1", 10*time.Millisecond)
	if !b.blocked("m1") {
		t.Error("blocked model not reported")
	}
	time.Sleep(20 * time.Millisecond)
	if b.blocked("m1") {
		t.Error("blocklist entry did not expire")
	}
	if !planBlockRe.MatchString("claude-opus-4-8 available in Pro and above plans or extra on demand usage") {
		t.Error("planBlockRe should match official access-denied message")
	}
	if planBlockRe.MatchString("rate limit exceeded") {
		t.Error("planBlockRe should not match rate limit errors")
	}
}
