package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePort(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"3050", 3050, true},
		{" 3000 ", 3000, true},
		// 0 视为"未指定"，走兜底
		{"0", 0, false},
		{"65535", 65535, true},
		{"tcp://0.0.0.0:3050", 3050, true},
		{"http://host:8080/base", 8080, true},
		{"[::]:9090", 9090, true},
		{"8080/tcp", 8080, true},
		{"", 0, false},
		{"abc", 0, false},
		{"99999", 0, false},
		{"-1", 0, false},
		{"tcp://host", 0, false},
	}
	for _, c := range cases {
		got, ok := parsePort(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("parsePort(%q) = %d,%v; want %d,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestParseHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.0.0.0", "0.0.0.0"},
		{" 127.0.0.1 ", "127.0.0.1"},
		{"tcp://10.1.2.3:3050", "10.1.2.3"},
		{"[::1]:9090", "::1"},
		{"", ""},
		// 无冒号的单 token 也按主机名接受
		{"myhost", "myhost"},
	}
	for _, c := range cases {
		got, ok := parseHost(c.in)
		wantOK := c.want != ""
		if ok != wantOK || (ok && got != c.want) {
			t.Errorf("parseHost(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, wantOK)
		}
	}
}

// envPortCandidates 扫描 PORT/CC_PORT 之外的 *_PORT 类环境变量
// （部署平台注入的模板变量等），作为端口兜底候选。
func TestEnvPortCandidates(t *testing.T) {
	t.Setenv("WEB_PORT", "tcp://0.0.0.0:7777")
	t.Setenv("PORT", "99999") // 非法，应被忽略
	cands := envPortCandidates()
	found := false
	for _, p := range cands {
		if p == 7777 {
			found = true
		}
		if p == 99999 {
			t.Errorf("invalid port 99999 leaked into candidates: %v", cands)
		}
	}
	if !found {
		t.Fatalf("envPortCandidates() = %v; want to contain 7777", cands)
	}
}

func TestExpandEnvRefs(t *testing.T) {
	t.Setenv("MY_PORT", "4050")
	if got := expandEnvRefs("${MY_PORT}"); got != "4050" {
		t.Errorf("expandEnvRefs = %q", got)
	}
	if got := expandEnvRefs("plain"); got != "plain" {
		t.Errorf("expandEnvRefs plain = %q", got)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(path)
	if cfg.APIBase != defaultAPIBase {
		t.Errorf("APIBase = %q", cfg.APIBase)
	}
	if cfg.StreamIdle != 30*time.Second || cfg.NonstreamIdle != 90*time.Second {
		t.Errorf("idle timeouts = %v / %v", cfg.StreamIdle, cfg.NonstreamIdle)
	}
	if cfg.MaxBodyBytes != 100<<20 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
}

func TestNormalizeCCUsageZeroOutput(t *testing.T) {
	u := &ccUsage{InputTokens: 500}
	normalizeCCUsage(u)
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("zero-output usage not normalized: %+v", u)
	}
	u2 := &ccUsage{InputTokens: 500, OutputTokens: 25}
	normalizeCCUsage(u2)
	if u2.InputTokens != 500 || u2.OutputTokens != 25 {
		t.Errorf("valid usage altered: %+v", u2)
	}
}

func TestAnthropicInputTokens(t *testing.T) {
	u := &ccUsage{InputTokens: 100, CachedInputTokens: 60}
	if got := anthropicInputTokens(u, -1); got != 40 {
		t.Errorf("anthropicInputTokens = %d; want 40 (non-cached only, issue #25)", got)
	}
}

func TestMapFinishReason(t *testing.T) {
	if mapFinishReason("stop") != "stop" || mapFinishReason("length") != "length" ||
		mapFinishReason("tool-calls") != "tool_calls" || mapFinishReason("") != "stop" ||
		mapFinishReason("weird") != "weird" {
		t.Error("mapFinishReason mapping broken")
	}
}

func TestKeyPoolFailover(t *testing.T) {
	cfgs := []UpstreamKeyConfig{{Key: "user_a"}, {Key: "user_b"}}
	pool := newKeyPool(cfgs, "round_robin")
	if pool.len() != 2 {
		t.Fatalf("pool.len = %d", pool.len())
	}
	k1 := pool.next(make([]bool, 2))
	k1.cooldown(429, 60*time.Second)
	// 冷却中的 key 不应再被选中
	k2 := pool.next(make([]bool, 2))
	if k2.Value == k1.Value {
		t.Errorf("cooled key %s selected again", k1.Value)
	}
	pool2 := newKeyPool([]UpstreamKeyConfig{{Key: "user_x"}}, "round_robin")
	if pool2.lookup("user_x") == nil {
		t.Error("lookup should find existing key")
	}
	if pool2.lookup("user_missing") != nil {
		t.Error("lookup should return nil for unknown key")
	}
}
