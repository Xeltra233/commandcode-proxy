package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ccVersionFallback = "1.53.0"
	// 上游响应头超时：比"读空闲"宽松得多，只用来兜住完全挂死的连接。
	upstreamHeaderTimeout = 5 * time.Minute
	// 单行 NDJSON 上限（超大 tool-call 参数），超过即判上游异常。
	maxCCLineBytes = 64 << 20
	// 初始化/模型列表这类小请求的独立超时
	shortRequestTimeout = 15 * time.Second
	// 真实 CLI 上报的环境串里的 Node 版本（保持与旧实现一致的指纹形态）
	nodeVersionForEnv = "v22.23.2"
)

var (
	errUpstreamIdle = errors.New("upstream idle timeout")
	errLineTooLong  = errors.New("upstream line too long")
)

var processWorkingDir = func() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "/"
}()

// ── 设备指纹（上游按 key 记录，首次 + 每 8h±2h 刷新） ──

type fingerprint struct {
	Thumbmark  string                `json:"thumbmark"`
	Components fingerprintComponents `json:"components"`
}

type fingerprintComponents struct {
	MachineIDHash    string   `json:"machineIdHash"`
	MacHashes        []string `json:"macHashes"`
	OSUserHash       string   `json:"osUserHash"`
	HostnameHash     string   `json:"hostnameHash"`
	GitEmailHash     string   `json:"gitEmailHash"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone"`
	Runtime          string   `json:"runtime"`
	CollectorVersion int      `json:"collectorVersion"`
}

var fingerprintCPUs = []struct {
	Model string
	Cores int
}{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var (
	fingerprintMems = []int{8, 16, 24, 32, 48, 64}
	fingerprintTZs  = []string{
		"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
		"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow",
		"Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong",
		"Australia/Sydney", "Pacific/Auckland",
	}
	fingerprintMacCounts = []int{2, 3, 4, 5}
)

func newFingerprint() *fingerprint {
	cpu := fingerprintCPUs[randInt63n(int64(len(fingerprintCPUs)))]
	mem := fingerprintMems[randInt63n(int64(len(fingerprintMems)))]
	tz := fingerprintTZs[randInt63n(int64(len(fingerprintTZs)))]
	macCount := fingerprintMacCounts[randInt63n(int64(len(fingerprintMacCounts)))]

	macHashes := make([]string, 0, macCount)
	for i := 0; i < macCount; i++ {
		macHashes = append(macHashes, sha256Hex(randHex(32)))
	}
	parts := fingerprintComponents{
		MachineIDHash:    sha256Hex(randHex(32)),
		MacHashes:        macHashes,
		OSUserHash:       sha256Hex(randHex(16)),
		HostnameHash:     sha256Hex(randHex(16)),
		GitEmailHash:     sha256Hex(randHex(16)),
		Platform:         "win32",
		Arch:             "x64",
		OSRelease:        "10.0.22631",
		CPUModel:         cpu.Model,
		CPUCount:         cpu.Cores,
		MemGiB:           mem,
		IsContainer:      false,
		Timezone:         tz,
		Runtime:          "cli",
		CollectorVersion: 1,
	}
	thumbParts := []string{parts.MachineIDHash}
	thumbParts = append(thumbParts, macHashes...)
	thumbParts = append(thumbParts,
		parts.OSUserHash, parts.HostnameHash, parts.GitEmailHash,
		"win32", "10.0.22631", cpu.Model, fmt.Sprint(cpu.Cores), fmt.Sprint(mem),
	)
	return &fingerprint{Thumbmark: sha256Hex(strings.Join(thumbParts, "|")), Components: parts}
}

// ── 上游客户端 ──────────────────────────────────────

type ccClient struct {
	cfg      *Config
	log      *logger
	stream   *http.Client // 流式：读空闲 = StreamIdle
	nonstrm  *http.Client // 非流式：读空闲 = NonstreamIdle
	short    *http.Client // 指纹/生命周期/模型列表
	version  atomic.Pointer[string]
	baseURL  string
	slugMode string // "fake" | "custom"

	modelsMu     sync.RWMutex
	modelsCache  []modelInfo
	modelsExpire time.Time

	planDetector *planDetector
}

type modelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func newCCClient(cfg *Config, log *logger) *ccClient {
	c := &ccClient{
		cfg:          cfg,
		log:          log,
		baseURL:      cfg.APIBase,
		stream:       newUpstreamClient(cfg, cfg.StreamIdle),
		nonstrm:      newUpstreamClient(cfg, cfg.NonstreamIdle),
		short:        newUpstreamClient(cfg, 0),
		slugMode:     "fake",
		planDetector: newPlanDetector(),
	}
	v := ccVersionFallback
	c.version.Store(&v)
	if cfg.ProjectSlug != "" && cfg.ProjectSlug != defaultSlug {
		c.slugMode = "custom"
	}
	return c
}

// newUpstreamClient 构造带连接池调优的客户端。
//
// 关键点：不使用全局 client.Timeout（长流会被整段掐断），改为在响应体上挂
// "每次 Read 的空闲计时器" —— 只有上游真的静默超时才中断，语义与旧实现一致，
// 且每次 Read 只做一次 timer.Reset，不新增 goroutine、不额外拷贝。
func newUpstreamClient(cfg *Config, idle time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          max(64, cfg.MaxIdleConnsPerHost*2),
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: upstreamHeaderTimeout,
		// 上游返回的是 NDJSON 流，压缩只会增加 CPU 与延迟
		DisableCompression: true,
		WriteBufferSize:    64 << 10,
		ReadBufferSize:     64 << 10,
		ForceAttemptHTTP2:  true,
	}
	return &http.Client{
		Transport: tr,
		// 不跟随重定向：上游是 API，重定向一律当作错误处理
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		// 空闲超时由 body 包装器负责
		Timeout: 0,
		// 兜底：idle 在 body 里实现，这里只是保持签名
		Jar: nil,
	}
}

func (c *ccClient) versionString() string {
	if v := c.version.Load(); v != nil {
		return *v
	}
	return ccVersionFallback
}

// refreshVersion 从 npm registry 拉真实 CLI 版本（24h 一次），失败保持现值。
func (c *ccClient) refreshVersion(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.npmjs.org/command-code/latest", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.short.Do(req)
	if err != nil {
		c.log.Warn("CC version fetch failed, using current", "version", c.versionString(), "error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("CC version fetch failed, using current", "version", c.versionString(), "status", resp.StatusCode)
		return
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pkg); err != nil || pkg.Version == "" {
		c.log.Warn("CC version fetch failed, using current", "version", c.versionString())
		return
	}
	v := pkg.Version
	c.version.Store(&v)
	c.log.Info("CC Version refreshed from npm", "version", v)
}

func (c *ccClient) startVersionRefresher(ctx context.Context) {
	go func() {
		c.refreshVersion(ctx)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.refreshVersion(ctx)
			}
		}
	}()
}

// ensureInitialized 首次使用某个 key 时补发 fingerprint / lifecycle 预请求
// （真实 CLI 启动时会做），之后每 8h±2h 刷新一次。
func (c *ccClient) ensureInitialized(ctx context.Context, key *UpstreamKey) {
	fp, due := key.fingerprintForInit(time.Now())
	if !due {
		return
	}
	initCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shortRequestTimeout)
	defer cancel()

	headers := func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-cli-environment", "production")
		req.Header.Set("Authorization", "Bearer "+key.Value)
		req.Header.Set("x-command-code-version", c.versionString())
		if c.cfg.ZDR {
			req.Header.Set("x-cmd-zdr", "1")
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		body, err := json.Marshal(fp)
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(initCtx, http.MethodPost, c.baseURL+"/alpha/fingerprint/record", bytes.NewReader(body))
		if err != nil {
			return
		}
		headers(req)
		resp, err := c.short.Do(req)
		if err != nil {
			if initCtx.Err() == nil {
				c.log.Warn("Fingerprint record error", "error", err.Error(), "key", key.label())
			}
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			c.log.Warn("Fingerprint record failed", "status", resp.StatusCode, "key", key.label())
		} else {
			c.log.Debug("Fingerprint recorded", "key", key.label())
		}
	}()

	go func() {
		defer wg.Done()
		payload := map[string]any{
			"eventType": "cli_session_exists",
			"metadata": map[string]any{
				"sessionId":  "sess_" + randHex(8),
				"cliVersion": c.versionString(),
				"mode":       "interactive",
				"os":         fp.Components.Platform + "-" + fp.Components.Arch,
			},
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(initCtx, http.MethodPost, c.baseURL+"/alpha/lifecycle-events", bytes.NewReader(body))
		if err != nil {
			return
		}
		headers(req)
		resp, err := c.short.Do(req)
		if err != nil {
			if initCtx.Err() == nil {
				c.log.Warn("Lifecycle event error", "error", err.Error(), "key", key.label())
			}
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			c.log.Warn("Lifecycle event failed", "status", resp.StatusCode, "key", key.label())
		} else {
			c.log.Debug("Lifecycle event sent", "key", key.label())
		}
	}()

	wg.Wait()
}

// generate 转发一次生成请求。/alpha/generate 永远返回 NDJSON 流。
func (c *ccClient) generate(ctx context.Context, key *UpstreamKey, body []byte, sessionID, projectSlug string, streaming, zdr bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+key.Value)
	h.Set("x-cli-environment", "production")
	h.Set("x-command-code-version", c.versionString())
	h.Set("x-session-id", sessionID)
	h.Set("x-co-flag", "false")
	h.Set("x-taste-learning", "false")
	h.Set("x-project-slug", projectSlug)
	h.Set("traceparent", generateTraceparent())
	if zdr {
		h.Set("x-cmd-zdr", "1")
	}
	client := c.nonstrm
	if streaming {
		client = c.stream
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body = newIdleBody(resp.Body, idleFor(c.cfg, streaming))
	key.requests.Add(1)
	return resp, nil
}

func idleFor(cfg *Config, streaming bool) time.Duration {
	if streaming {
		return cfg.StreamIdle
	}
	return cfg.NonstreamIdle
}

// projectSlugFor 计算 x-project-slug：显式配置优先，否则按旧实现由 session 派生。
func (c *ccClient) projectSlugFor(sessionID string) string {
	if c.slugMode == "custom" {
		return c.cfg.ProjectSlug
	}
	return fakeProjectSlug(sessionID)
}

// ── 模型列表 ────────────────────────────────────────

// fallbackModels 见 fallback_models.go（官方目录快照，按套餐过滤见 plan.go）。

func (c *ccClient) models(ctx context.Context, key *UpstreamKey) []modelInfo {
	now := time.Now()
	c.modelsMu.RLock()
	if c.modelsCache != nil && now.Before(c.modelsExpire) {
		out := c.modelsCache
		c.modelsMu.RUnlock()
		return out
	}
	c.modelsMu.RUnlock()

	list, ok := c.fetchModels(ctx, key)
	if !ok {
		c.modelsMu.RLock()
		cached := c.modelsCache
		c.modelsMu.RUnlock()
		if cached != nil {
			return cached
		}
		return fallbackModels
	}
	c.modelsMu.Lock()
	c.modelsCache = list
	c.modelsExpire = now.Add(c.cfg.ModelRefreshInterval)
	c.modelsMu.Unlock()
	return list
}

func (c *ccClient) fetchModels(ctx context.Context, key *UpstreamKey) ([]modelInfo, bool) {
	if key == nil || !c.cfg.UseProviderModels {
		return nil, false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+key.Value)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-command-code-version", c.versionString())
	resp, err := c.short.Do(req)
	if err != nil {
		c.log.Warn("Provider models fetch error, using cached list", "error", err.Error())
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		c.log.Warn("Provider models fetch failed, using cached list", "status", resp.StatusCode)
		return nil, false
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil || len(payload.Data) == 0 {
		return nil, false
	}
	out := make([]modelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, modelInfo{ID: m.ID, Name: m.ID})
	}
	if len(out) == 0 {
		return nil, false
	}
	c.log.Info("Fetched models from Provider API", "count", len(out))
	return out, true
}

// ── 上游 NDJSON 读取 ────────────────────────────────

var lineReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 128<<10) },
}

// readCCLines 按行回调上游 NDJSON；回调期间行缓冲有效，返回后即失效。
// 使用 ReadSlice 而不是 Scanner：无逐行分配、无 Scanner 的 token 上限问题，
// 超长行（大 tool-call 参数）走 scratch 累积，整体 O(n)。
func readCCLines(r io.Reader, fn func(line []byte) error) error {
	br := lineReaderPool.Get().(*bufio.Reader)
	defer func() {
		br.Reset(nil)
		lineReaderPool.Put(br)
	}()
	br.Reset(r)

	var scratch []byte
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			scratch = append(scratch[:0], line...)
			for errors.Is(err, bufio.ErrBufferFull) {
				if len(scratch) > maxCCLineBytes {
					return errLineTooLong
				}
				line, err = br.ReadSlice('\n')
				scratch = append(scratch, line...)
			}
			line = scratch
		}
		if len(line) > 0 {
			if cbErr := fn(trimLine(line)); cbErr != nil {
				return cbErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// idleBody 给响应体挂"每次 Read 的空闲超时"。
//
// 为什么不用 conn.SetReadDeadline：连接是复用的，池里的空闲连接会带着过期的
// deadline 被下一个请求捡起来。body 级计时器只在读等待期间生效，且每个请求
// 只创建一个 timer，Reset/Stop 是纳秒级操作，不引入额外 goroutine 与拷贝。
type idleBody struct {
	rc       io.ReadCloser
	idle     time.Duration
	timer    *time.Timer
	timedOut atomic.Bool
	closed   atomic.Bool
}

func newIdleBody(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleBody{rc: rc, idle: idle}
	b.timer = time.AfterFunc(idle, func() {
		b.timedOut.Store(true)
		_ = b.rc.Close()
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && b.timedOut.Load() {
		return n, errUpstreamIdle
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	if b.closed.CompareAndSwap(false, true) {
		return b.rc.Close()
	}
	return nil
}

// drainAndClose 读掉少量剩余字节后关闭：让连接有机会被复用，又不浪费带宽。
func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, rc, 64<<10)
	_ = rc.Close()
}

// ── 工具 ────────────────────────────────────────────

var (
	fakeSlugNames = []string{"app", "api", "backend", "bot", "cli", "core", "data", "frontend",
		"lib", "plugin", "proxy", "server", "service", "tool", "web", "worker"}
)

// fakeProjectSlug 由 sessionId 派生一个形如 "d-users-dev-projects-app-a3f2" 的 slug
// （与真实 CLI 的路径 slug 规则一致）。
func fakeProjectSlug(sessionID string) string {
	id := sessionID
	head := id
	if len(head) > 4 {
		head = head[:4]
	}
	var idx uint64
	if parsed, err := parseHexPrefix(head); err == nil {
		idx = parsed
	} else {
		// sessionId 也可能是客户端自定义的 prompt_cache_key（非十六进制）
		var h uint32
		for i := 0; i < len(id); i++ {
			h = h*31 + uint32(id[i])
		}
		idx = uint64(h)
	}
	name := fakeSlugNames[idx%uint64(len(fakeSlugNames))]
	suffix := head
	if suffix == "" {
		suffix = "0000"
	}
	path := `C:\Users\dev\projects\` + name + "-" + suffix
	var sb strings.Builder
	sb.Grow(len(path))
	lower := strings.ToLower(path)
	if len(lower) > 1 && lower[1] == ':' {
		lower = lower[2:]
	}
	prevDash := false
	for i := 0; i < len(lower); i++ {
		ch := lower[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			sb.WriteByte(ch)
			prevDash = false
			continue
		}
		if !prevDash && sb.Len() > 0 {
			sb.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(sb.String(), "-")
}

func parseHexPrefix(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return 0, errors.New("not hex")
		}
		v = v*16 + d
	}
	return v, nil
}

func generateTraceparent() string {
	return "00-" + randHex(16) + "-" + randHex(8) + "-01"
}

func environmentString() string {
	osName := runtime.GOOS
	switch osName {
	case "windows":
		osName = "win32"
	case "darwin":
		osName = "darwin"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	case "arm64":
		arch = "arm64"
	}
	return osName + "-" + arch + ", Node.js " + nodeVersionForEnv
}

func todayString() string {
	return time.Now().Format("2006-01-02")
}
