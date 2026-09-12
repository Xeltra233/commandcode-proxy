package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort    = 3000
	defaultAPIBase = "https://api.commandcode.ai"
	defaultSlug    = "cc-proxy"
	maxPort        = 65535
)

// 端口绑不上时按顺序兜底尝试；镜像/编排器的默认端口都在这里，
// 保证任何环境都能绑到一个"有意义的"端口而不是直接退出。
var fallbackPorts = []int{3050, 3000, 8080}

type UpstreamKeyConfig struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type ClientKeyConfig struct {
	Key string `json:"key"`
	// Name 仅用于日志中标识调用方
	Name string `json:"name"`
	// UpstreamKey / UpstreamKeyIndex 可把某个下游 key 固定绑到某个上游 key
	// （多租户常见需求：每个下游 key 走自己的上游额度）。都不填则走上游池轮询。
	UpstreamKey   string `json:"upstreamKey,omitempty"`
	UpstreamIndex *int   `json:"upstreamKeyIndex,omitempty"`
}

// Config 是进程的最终配置：config.json → 环境变量 → 默认值。
type Config struct {
	Port        int
	Host        string
	APIBase     string
	ProjectSlug string

	UpstreamKeys []UpstreamKeyConfig
	ClientKeys   []ClientKeyConfig

	// UpstreamProxy：上游出口代理（http/https/socks5 URL）。空 = 走标准
	// HTTPS_PROXY 环境变量；都不设则直连。用于部署机 IP 被上游
	// Cloudflare bot 规则拦截时换出口。
	UpstreamProxy string

	LogFile  string
	LogLevel string

	UseProviderModels      bool
	ModelRefreshInterval   time.Duration
	ZDR                    bool
	EmptySystemPlaceholder bool

	KeyStrategy string
	KeyFailover int
	KeyCooldown time.Duration
	// AllowBYOKeys：是否允许客户端自带上游 key（user_xxx）。nil = 未显式配置，
	// 由"是否配置了 clientKeys"决定（配置了分发 key 就默认关闭）。
	AllowBYOKeys *bool
	// Plan：订阅套餐等级（go/goat/pro/provider/max）。
	// go/goat 会把 premium 模型从模型列表里滤掉，保证列表返回的都能用；
	// 空或 "all" 表示不过滤（默认）。
	Plan string
	// PlanPremiumModels：当前套餐下显式可用的 premium 模型
	// （上游的 per-model allowance 名单未公开，需要手动补充）。
	PlanPremiumModels []string

	MaxBodyBytes  int64
	StreamIdle    time.Duration
	NonstreamIdle time.Duration
	MaxInflight   int64
	ClientStall   time.Duration

	MaxIdleConnsPerHost int
	MaxConnsPerHost     int

	ShutdownTimeout time.Duration
	CCVersion       string

	ConfigPath string
}

// fileConfig 里所有可选字段都用指针，用来区分"没写"和"写了零值"。
type fileConfig struct {
	Port        *int    `json:"port"`
	Host        *string `json:"host"`
	APIBase     *string `json:"apiBase"`
	ProjectSlug *string `json:"projectSlug"`

	UpstreamKeys []UpstreamKeyConfig `json:"upstreamKeys"`
	UpstreamKey  *string             `json:"upstreamKey"`
	APIKeys      []UpstreamKeyConfig `json:"apiKeys"`
	APIKey       *string             `json:"apiKey"`

	ClientKeys []ClientKeyConfig `json:"clientKeys"`
	// 兼容：proxyKeys / downstreamKeys 都指向 clientKeys
	ProxyKeys      []ClientKeyConfig `json:"proxyKeys"`
	DownstreamKeys []ClientKeyConfig `json:"downstreamKeys"`

	LogFile  *string `json:"logFile"`
	LogLevel *string `json:"logLevel"`

	UseProviderModels      *bool `json:"useProviderModels"`
	ModelRefreshIntervalMs *int  `json:"modelRefreshIntervalMs"`
	ZDR                    *bool `json:"zdr"`
	EmptySystemPlaceholder *bool `json:"emptySystemPlaceholder"`

	Plan                *string  `json:"plan"`
	PlanPremiumModels   []string `json:"planPremiumModels"`
	AllowBYOKeys        *bool    `json:"allowByoKeys"`
	KeyStrategy         *string  `json:"keyStrategy"`
	KeyFailover         *int     `json:"keyFailover"`
	KeyCooldownMs       *int     `json:"keyCooldownMs"`
	MaxIdleConnsPerHost *int     `json:"maxIdleConnsPerHost"`
	MaxConnsPerHost     *int     `json:"maxConnsPerHost"`
	MaxBodyMB           *int     `json:"maxBodyMB"`
	MaxInflight         *int     `json:"maxInflight"`
	StreamIdleMs        *int     `json:"streamIdleMs"`
	NonstreamIdleMs     *int     `json:"nonstreamIdleMs"`
	ClientStallMs       *int     `json:"clientStallMs"`
	ShutdownTimeoutMs   *int     `json:"shutdownTimeoutMs"`
}

var configWarnings []string

func warnConfig(format string, args ...any) {
	configWarnings = append(configWarnings, fmt.Sprintf(format, args...))
}

// ── 端口 / 监听地址解析 ─────────────────────────────
// 编排器注入的 PORT 写法并不统一：纯数字、带空白或引号、"tcp://0.0.0.0:3050"、
// "0.0.0.0:3050"、"[::]:3050"、"8080/tcp"，甚至是没被展开的模板变量（"${WEB_PORT}"）。
// 解析失败时绝不 panic、也不拿着非法值去 bind（Node 版本就是在这里抛
// ERR_SOCKET_BAD_PORT 崩溃重启），而是回落默认值 + 记警告 + 绑定时换候选。
func parsePort(v string) (int, bool) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return 0, false
	}
	if p, err := strconv.Atoi(raw); err == nil {
		return validPort(p)
	}
	// host:port / scheme://host:port / scheme://host:port/path
	if m := rePortTail.FindStringSubmatch(raw); m != nil {
		if p, err := strconv.Atoi(m[1]); err == nil {
			return validPort(p)
		}
	}
	// "8080/tcp"、"port=8080" 这类只含一个数字的写法
	if nums := reDigits.FindAllString(raw, -1); len(nums) == 1 {
		if p, err := strconv.Atoi(nums[0]); err == nil {
			return validPort(p)
		}
	}
	return 0, false
}

var (
	rePortTail = regexp.MustCompile(`:(\d{1,5})(?:/.*)?$`)
	reDigits   = regexp.MustCompile(`\d{1,5}`)
	reHostOK   = regexp.MustCompile(`^[0-9a-zA-Z._:-]+$`)
	reScheme   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	reEnvRef   = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)
)

func validPort(p int) (int, bool) {
	if p >= 1 && p <= maxPort {
		return p, true
	}
	return 0, false
}

// parseHost 容忍 URL / host:port 形式的 HOST（非法值回落默认，不崩）。
func parseHost(v string) (string, bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", false
	}
	s = reScheme.ReplaceAllString(s, "")
	if i := strings.IndexAny(s, "/"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end > 0 {
			s = s[1:end]
		}
	} else if strings.Count(s, ":") == 1 {
		if head, tail, ok := strings.Cut(s, ":"); ok {
			if _, err := strconv.Atoi(tail); err == nil {
				s = head
			}
		}
	}
	if s == "" || strings.ContainsAny(s, " \t\r\n") || !reHostOK.MatchString(s) {
		return "", false
	}
	return s, true
}

// expandEnvRefs 展开 "${VAR}" / "$VAR"：平台把模板变量原样注入时的兜底，
// 未定义的引用保持原样（便于日志里看出问题）。
func expandEnvRefs(raw string) string {
	if !strings.Contains(raw, "$") {
		return raw
	}
	for depth := 0; depth < 3; depth++ {
		next := reEnvRef.ReplaceAllStringFunc(raw, func(ref string) string {
			name := strings.TrimPrefix(strings.TrimPrefix(ref, "${"), "$")
			name = strings.TrimSuffix(name, "}")
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			return ref
		})
		if next == raw {
			return raw
		}
		raw = next
	}
	return raw
}

// envPortCandidates 收集环境里其它"看起来像端口"的变量（PORT 不可用时才用得上）：
// 名字含 port 且值能解析成合法端口，*_PORT 优先，其次 PORT_*。
func envPortCandidates() []int {
	type cand struct {
		name string
		port int
	}
	var cands []cand
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "PORT" || name == "CC_PORT" {
			continue
		}
		if !strings.Contains(strings.ToLower(name), "port") {
			continue
		}
		if p, ok := parsePort(value); ok {
			cands = append(cands, cand{name: name, port: p})
		}
	}
	rank := func(n string) int {
		lower := strings.ToLower(n)
		if strings.HasSuffix(lower, "_port") || lower == "port" {
			return 0
		}
		if strings.HasPrefix(lower, "port_") {
			return 1
		}
		return 2
	}
	// 稳定排序：rank → 名字长度
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0; j-- {
			a, b := cands[j-1], cands[j]
			if rank(b.name) < rank(a.name) || (rank(b.name) == rank(a.name) && len(b.name) < len(a.name)) {
				cands[j-1], cands[j] = cands[j], cands[j-1]
				continue
			}
			break
		}
	}
	out := make([]int, 0, len(cands))
	seen := map[int]bool{}
	for _, c := range cands {
		if !seen[c.port] {
			seen[c.port] = true
			out = append(out, c.port)
		}
	}
	return out
}

// ── 环境变量取值 ────────────────────────────────────

func envStr(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// splitList 支持逗号 / 分号 / 空白 / 换行分隔，便于把 key 列表塞进单个环境变量。
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseUpstreamKeys(raw string) []UpstreamKeyConfig {
	var out []UpstreamKeyConfig
	for _, k := range splitList(raw) {
		out = append(out, UpstreamKeyConfig{Key: k})
	}
	return out
}

func parseClientKeys(raw string) []ClientKeyConfig {
	var out []ClientKeyConfig
	for _, k := range splitList(raw) {
		out = append(out, ClientKeyConfig{Key: k})
	}
	return out
}

// dedupeUpstreamKeys 去重，保留先出现的顺序（同一把 key 配两次会导致池内空转）。
func dedupeUpstreamKeys(keys []UpstreamKeyConfig) []UpstreamKeyConfig {
	seen := map[string]bool{}
	out := keys[:0]
	for _, k := range keys {
		k.Key = strings.TrimSpace(k.Key)
		if k.Key == "" || seen[k.Key] {
			continue
		}
		seen[k.Key] = true
		out = append(out, k)
	}
	return out
}

func dedupeClientKeys(keys []ClientKeyConfig) []ClientKeyConfig {
	seen := map[string]bool{}
	out := keys[:0]
	for _, k := range keys {
		k.Key = strings.TrimSpace(k.Key)
		if k.Key == "" || seen[k.Key] {
			continue
		}
		seen[k.Key] = true
		out = append(out, k)
	}
	return out
}

func envBool(name string) (bool, bool) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off", "":
		return false, true
	default:
		return false, false
	}
}

func envInt(name string) (int, bool) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		warnConfig("环境变量 %s=%q 不是整数，已忽略", name, v)
		return 0, false
	}
	return n, true
}

func intPtr(v int) *int { return &v }

// ── 加载 ────────────────────────────────────────────

func loadConfig(configPath string) *Config {
	cfg := &Config{
		Port:                   defaultPort,
		Host:                   "0.0.0.0",
		APIBase:                defaultAPIBase,
		ProjectSlug:            defaultSlug,
		LogLevel:               "info",
		UseProviderModels:      true,
		ModelRefreshInterval:   5 * time.Minute,
		EmptySystemPlaceholder: true,
		KeyStrategy:            "round_robin",
		KeyFailover:            1,
		KeyCooldown:            60 * time.Second,
		MaxBodyBytes:           100 << 20,
		StreamIdle:             30 * time.Second,
		NonstreamIdle:          90 * time.Second,
		MaxInflight:            0,
		ClientStall:            0,
		MaxIdleConnsPerHost:    512,
		MaxConnsPerHost:        0,
		ShutdownTimeout:        10 * time.Second,
		CCVersion:              ccVersionFallback,
		ConfigPath:             configPath,
	}

	var fc fileConfig
	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			if err := json.Unmarshal(data, &fc); err != nil {
				warnConfig("config.json 解析失败（已忽略）：%v", err)
			}
		} else if !os.IsNotExist(err) {
			warnConfig("config.json 读取失败（已忽略）：%v", err)
		}
	}

	// config.json
	if fc.Port != nil {
		cfg.Port = *fc.Port
	}
	if fc.Host != nil {
		cfg.Host = *fc.Host
	}
	if fc.APIBase != nil && *fc.APIBase != "" {
		cfg.APIBase = strings.TrimRight(*fc.APIBase, "/")
	}
	if fc.ProjectSlug != nil && *fc.ProjectSlug != "" {
		cfg.ProjectSlug = *fc.ProjectSlug
	}
	cfg.UpstreamKeys = append(cfg.UpstreamKeys, fc.UpstreamKeys...)
	if fc.UpstreamKey != nil {
		cfg.UpstreamKeys = append(cfg.UpstreamKeys, UpstreamKeyConfig{Key: *fc.UpstreamKey})
	}
	cfg.UpstreamKeys = append(cfg.UpstreamKeys, fc.APIKeys...)
	if fc.APIKey != nil {
		cfg.UpstreamKeys = append(cfg.UpstreamKeys, UpstreamKeyConfig{Key: *fc.APIKey})
	}
	cfg.ClientKeys = append(cfg.ClientKeys, fc.ClientKeys...)
	cfg.ClientKeys = append(cfg.ClientKeys, fc.ProxyKeys...)
	cfg.ClientKeys = append(cfg.ClientKeys, fc.DownstreamKeys...)
	if fc.LogFile != nil {
		cfg.LogFile = *fc.LogFile
	}
	if fc.LogLevel != nil && *fc.LogLevel != "" {
		cfg.LogLevel = *fc.LogLevel
	}
	if fc.UseProviderModels != nil {
		cfg.UseProviderModels = *fc.UseProviderModels
	}
	if fc.ModelRefreshIntervalMs != nil && *fc.ModelRefreshIntervalMs > 0 {
		cfg.ModelRefreshInterval = time.Duration(*fc.ModelRefreshIntervalMs) * time.Millisecond
	}
	if fc.ZDR != nil {
		cfg.ZDR = *fc.ZDR
	}
	if fc.EmptySystemPlaceholder != nil {
		cfg.EmptySystemPlaceholder = *fc.EmptySystemPlaceholder
	}
	if fc.Plan != nil {
		cfg.Plan = normalizePlan(*fc.Plan)
	}
	cfg.PlanPremiumModels = fc.PlanPremiumModels
	if fc.AllowBYOKeys != nil {
		v := *fc.AllowBYOKeys
		cfg.AllowBYOKeys = &v
	}
	if fc.KeyStrategy != nil && *fc.KeyStrategy != "" {
		cfg.KeyStrategy = *fc.KeyStrategy
	}
	if fc.KeyFailover != nil && *fc.KeyFailover >= 0 {
		cfg.KeyFailover = *fc.KeyFailover
	}
	if fc.KeyCooldownMs != nil && *fc.KeyCooldownMs >= 0 {
		cfg.KeyCooldown = time.Duration(*fc.KeyCooldownMs) * time.Millisecond
	}
	if fc.MaxIdleConnsPerHost != nil && *fc.MaxIdleConnsPerHost > 0 {
		cfg.MaxIdleConnsPerHost = *fc.MaxIdleConnsPerHost
	}
	if fc.MaxConnsPerHost != nil && *fc.MaxConnsPerHost > 0 {
		cfg.MaxConnsPerHost = *fc.MaxConnsPerHost
	}
	if fc.MaxBodyMB != nil && *fc.MaxBodyMB > 0 {
		cfg.MaxBodyBytes = int64(*fc.MaxBodyMB) << 20
	}
	if fc.MaxInflight != nil && *fc.MaxInflight > 0 {
		cfg.MaxInflight = int64(*fc.MaxInflight)
	}
	if fc.StreamIdleMs != nil && *fc.StreamIdleMs > 0 {
		cfg.StreamIdle = time.Duration(*fc.StreamIdleMs) * time.Millisecond
	}
	if fc.NonstreamIdleMs != nil && *fc.NonstreamIdleMs > 0 {
		cfg.NonstreamIdle = time.Duration(*fc.NonstreamIdleMs) * time.Millisecond
	}
	if fc.ClientStallMs != nil && *fc.ClientStallMs > 0 {
		cfg.ClientStall = time.Duration(*fc.ClientStallMs) * time.Millisecond
	}
	if fc.ShutdownTimeoutMs != nil && *fc.ShutdownTimeoutMs > 0 {
		cfg.ShutdownTimeout = time.Duration(*fc.ShutdownTimeoutMs) * time.Millisecond
	}

	// 环境变量（优先级最高）
	rawPort, hasPortEnv := envStr("PORT", "CC_PORT")
	if hasPortEnv {
		resolved := expandEnvRefs(rawPort)
		if p, ok := parsePort(resolved); ok {
			cfg.Port = p
		} else {
			shown := resolved
			if len(shown) > 64 {
				shown = shown[:64] + "…"
			}
			warnConfig("PORT=%q 不是合法端口（已忽略），回落 %d；绑不上时会依次尝试 %v",
				shown, cfg.Port, fallbackPorts)
		}
	}
	if v, ok := envStr("HOST"); ok {
		if h, ok := parseHost(v); ok {
			cfg.Host = h
		} else {
			warnConfig("HOST=%q 不是合法监听地址（已忽略），回落 %s", v, cfg.Host)
		}
	}
	if v, ok := envStr("CC_API_BASE"); ok {
		cfg.APIBase = strings.TrimRight(v, "/")
	}
	if v, ok := envStr("PROJECT_SLUG"); ok {
		cfg.ProjectSlug = v
	}
	if v, ok := envStr("CC_UPSTREAM_PROXY"); ok {
		cfg.UpstreamProxy = v
	}
	if v, ok := envStr("CC_UPSTREAM_KEYS", "CC_API_KEYS", "CC_API_KEY"); ok {
		cfg.UpstreamKeys = parseUpstreamKeys(v)
	}
	if v, ok := envStr("CC_CLIENT_KEYS", "PROXY_API_KEYS", "CC_PROXY_KEYS"); ok {
		cfg.ClientKeys = parseClientKeys(v)
	}
	if v, ok := envStr("LOG_FILE"); ok {
		cfg.LogFile = v
	}
	if v, ok := envStr("LOG_LEVEL", "CC_LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}
	if b, ok := envBool("CC_USE_PROVIDER_MODELS"); ok {
		cfg.UseProviderModels = b
	}
	if v, ok := envInt("CC_MODEL_REFRESH_MS"); ok && v > 0 {
		cfg.ModelRefreshInterval = time.Duration(v) * time.Millisecond
	}
	if b, ok := envBool("CMD_ZDR"); ok {
		cfg.ZDR = b
	}
	if b, ok := envBool("CC_EMPTY_SYSTEM_PLACEHOLDER"); ok {
		cfg.EmptySystemPlaceholder = b
	}
	if v, ok := envStr("CC_PLAN"); ok {
		cfg.Plan = normalizePlan(v)
	}
	if v, ok := envStr("CC_PLAN_PREMIUM_MODELS"); ok {
		cfg.PlanPremiumModels = splitList(v)
	}
	if b, ok := envBool("CC_ALLOW_BYO"); ok {
		v := b
		cfg.AllowBYOKeys = &v
	}
	if v, ok := envStr("CC_KEY_STRATEGY"); ok {
		cfg.KeyStrategy = v
	}
	if v, ok := envInt("CC_KEY_FAILOVER"); ok && v >= 0 {
		cfg.KeyFailover = v
	}
	if v, ok := envInt("CC_KEY_COOLDOWN_MS"); ok && v >= 0 {
		cfg.KeyCooldown = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt("CC_MAX_BODY_MB"); ok && v > 0 {
		cfg.MaxBodyBytes = int64(v) << 20
	}
	if v, ok := envInt("CC_STREAM_IDLE_MS"); ok && v > 0 {
		cfg.StreamIdle = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt("CC_NONSTREAM_IDLE_MS"); ok && v > 0 {
		cfg.NonstreamIdle = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt("CC_MAX_INFLIGHT"); ok && v > 0 {
		cfg.MaxInflight = int64(v)
	}
	if v, ok := envInt("CC_CLIENT_STALL_MS"); ok && v > 0 {
		cfg.ClientStall = time.Duration(v) * time.Millisecond
	} else if v, ok := envInt("CC_CLIENT_DRAIN_TIMEOUT_MS"); ok && v > 0 {
		cfg.ClientStall = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt("CC_MAX_IDLE_CONNS_PER_HOST"); ok && v > 0 {
		cfg.MaxIdleConnsPerHost = v
	}
	if v, ok := envInt("CC_MAX_CONNS_PER_HOST"); ok && v > 0 {
		cfg.MaxConnsPerHost = v
	}
	if v, ok := envInt("CC_SHUTDOWN_TIMEOUT_MS"); ok && v > 0 {
		cfg.ShutdownTimeout = time.Duration(v) * time.Millisecond
	}

	// 归一化 + 校验
	if p, ok := validPort(cfg.Port); ok {
		cfg.Port = p
	} else {
		warnConfig("端口 %d 不在 1..65535，已改用 %d", cfg.Port, defaultPort)
		cfg.Port = defaultPort
	}
	if h, ok := parseHost(cfg.Host); ok {
		cfg.Host = h
	} else {
		warnConfig("监听地址 %q 非法，已改用 0.0.0.0", cfg.Host)
		cfg.Host = "0.0.0.0"
	}
	cfg.UpstreamKeys = dedupeUpstreamKeys(cfg.UpstreamKeys)
	cfg.ClientKeys = dedupeClientKeys(cfg.ClientKeys)
	switch cfg.KeyStrategy {
	case "round_robin", "random":
	default:
		warnConfig("keyStrategy=%q 未知，已改用 round_robin", cfg.KeyStrategy)
		cfg.KeyStrategy = "round_robin"
	}
	if cfg.MaxBodyBytes < 1<<20 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.LogFile == "" {
		cfg.LogFile = ""
	}
	return cfg
}

// listenCandidates 生成监听候选：配置端口 → 环境里其它 *_PORT → 常见默认端口。
// 第一个候选来自配置；后续候选只在绑不上（占用/权限/地址不可用）时才生效。
func (c *Config) listenCandidates() []listenTarget {
	var out []listenTarget
	seen := map[string]bool{}
	add := func(port int, host, source string) {
		key := fmt.Sprintf("%s:%d", host, port)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, listenTarget{Port: port, Host: host, Source: source})
	}
	add(c.Port, c.Host, "configured")
	for _, p := range envPortCandidates() {
		add(p, c.Host, "env")
	}
	if c.Host != "0.0.0.0" {
		add(c.Port, "0.0.0.0", "configured, all interfaces")
	}
	for _, p := range fallbackPorts {
		add(p, "0.0.0.0", "fallback")
	}
	return out
}

type listenTarget struct {
	Port   int
	Host   string
	Source string
}

func (t listenTarget) String() string {
	return fmt.Sprintf("%s:%d", t.Host, t.Port)
}

// configPathDefault 取可执行文件同目录的 config.json（容器里就是 /app/config.json）。
func configPathDefault() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}
