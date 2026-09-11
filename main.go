package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	configPath := configPathDefault()
	if v, ok := envStr("CC_CONFIG"); ok {
		configPath = v
	}
	flag.StringVar(&configPath, "config", configPath, "path to config.json")
	flag.Parse()

	cfg := loadConfig(configPath)
	log := newLogger(cfg.LogLevel, cfg.LogFile)
	for _, warning := range configWarnings {
		log.Warn("Config value ignored", "detail", warning)
	}

	pool := newKeyPool(cfg.UpstreamKeys, cfg.KeyStrategy)
	allowBYO := len(cfg.ClientKeys) == 0
	if cfg.AllowBYOKeys != nil {
		allowBYO = *cfg.AllowBYOKeys
	}
	auth, authWarnings := newAuthenticator(cfg.ClientKeys, pool, allowBYO)
	for _, warning := range authWarnings {
		log.Warn("Client key config issue", "detail", warning)
	}

	cc := newCCClient(cfg, log)
	ps := &proxyServer{
		cfg:     cfg,
		log:     log,
		cc:      cc,
		keys:    pool,
		auth:    auth,
		limiter: newInflightLimiter(cfg.MaxInflight),
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cc.startVersionRefresher(rootCtx)
	go auth.janitor(rootCtx)

	srv := &http.Server{
		Handler: ps,
		// 只设置头读取超时；读/写超时留空，否则长流会被整段掐断
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ln, err := listenWithFallback(rootCtx, cfg, log)
	if err != nil {
		log.Error("Failed to start listener", "error", err.Error())
		os.Exit(1)
	}
	logStartupBanner(log, cfg, pool, auth, ln)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-rootCtx.Done():
		log.Info("Shutting down", "timeoutMs", cfg.ShutdownTimeout.Milliseconds())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("Graceful shutdown incomplete, closing", "error", err.Error())
			_ = srv.Close()
		}
		log.Info("Stopped")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Server error", "message", err.Error())
			os.Exit(1)
		}
	}
}

// listenWithFallback 依次尝试候选地址；全部失败时保持进程存活并周期性重试。
//
// 旧实现里非法端口会让进程在启动瞬间崩溃，被编排器反复重启（崩溃重试循环）。
// 这里的原则是：端口问题永远不让进程退出 —— 要么绑到候选端口上，要么原地重试。
func listenWithFallback(ctx context.Context, cfg *Config, log *logger) (net.Listener, error) {
	targets := cfg.listenCandidates()
	addrs := make([]string, 0, len(targets))
	for _, t := range targets {
		addrs = append(addrs, t.String())
	}
	for round := 1; ; round++ {
		for i, t := range targets {
			ln, err := net.Listen("tcp", t.String())
			if err == nil {
				log.Info("Binding", "addr", t.String(), "source", t.Source, "candidate", fmt.Sprintf("%d/%d", i+1, len(targets)))
				return ln, nil
			}
			log.Warn("Cannot bind, trying next candidate",
				"addr", t.String(), "source", t.Source, "error", err.Error(),
				"candidate", fmt.Sprintf("%d/%d", i+1, len(targets)))
		}
		if round <= 3 || round%12 == 0 {
			log.Error("All listen candidates failed; process stays alive and keeps retrying",
				"tried", strings.Join(addrs, ", "), "round", round, "retryInMs", 5000)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func logStartupBanner(log *logger, cfg *Config, pool *keyPool, auth *authenticator, ln net.Listener) {
	zdr := "off (CMD_ZDR=1 or per-request x-cmd-zdr: 1 to enable)"
	if cfg.ZDR {
		zdr = "enabled (x-cmd-zdr: 1 on generation/init requests)"
	}
	placeholder := "off"
	if cfg.EmptySystemPlaceholder {
		placeholder = "on (space placeholder for requests without system prompt, issue #17)"
	}
	maxInflight := "unlimited (CC_MAX_INFLIGHT=0)"
	if cfg.MaxInflight > 0 {
		maxInflight = fmt.Sprintf("%d (global, /health exempt)", cfg.MaxInflight)
	}
	authMode := "pass-through (no clientKeys configured: request key is used upstream)"
	if auth.enabled {
		authMode = fmt.Sprintf("%d distributed client key(s), upstream key from pool", len(auth.keys))
	}
	log.Info("CC Proxy started",
		"url", "http://"+ln.Addr().String(),
		"api", cfg.APIBase,
		"upstreamKeys", pool.len(),
		"downstreamAuth", authMode,
		"keyStrategy", cfg.KeyStrategy,
		"keyFailover", cfg.KeyFailover,
		"keyCooldownMs", cfg.KeyCooldown.Milliseconds(),
		"zdr", zdr,
		"emptySystemPlaceholder", placeholder,
		"logFile", orFallback(cfg.LogFile, "(console only)"),
		"idleTimeouts", fmt.Sprintf("stream %s / nonstream %s", prettyMs(cfg.StreamIdle), prettyMs(cfg.NonstreamIdle)),
		"clientStallTimeout", prettyMs(cfg.ClientStall),
		"maxInflight", maxInflight,
		"maxBodyMB", cfg.MaxBodyBytes>>20,
		"maxIdleConnsPerHost", cfg.MaxIdleConnsPerHost,
	)
	if pool.len() == 0 {
		log.Warn("No upstream API key configured; requests must carry their own key (user_xxx) or the proxy returns 503")
	}
}

func orFallback(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ── 路由 ────────────────────────────────────────────

// ServeHTTP 是全部请求入口：CORS → 探活 → 在途上限 → 路由。
func (s *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "*")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 探活与编排器健康检查永不因业务繁忙被拒
	if r.URL.Path == "/health" || r.URL.Path == "/" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}

	if !s.limiter.Acquire() {
		s.log.Warn("In-flight limit reached, rejecting request",
			"maxInflight", s.cfg.MaxInflight, "inflight", s.limiter.Current(), "path", r.URL.Path)
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_busy",
			fmt.Sprintf("Too many concurrent requests (limit %d), retry shortly", s.cfg.MaxInflight), 5)
		return
	}
	defer s.limiter.Release()

	// 单个请求 panic 不能带走整个进程
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("Panic in handler", "path", r.URL.Path, "panic", fmt.Sprintf("%v", rec))
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "Internal server error", 0)
		}
	}()

	switch {
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.handleChatCompletions(w, r)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		s.handleMessages(w, r)
	case r.URL.Path == "/v1/responses" && r.Method == http.MethodPost:
		s.handleResponses(w, r)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1beta/models/") && r.Method == http.MethodPost:
		model, action, ok := parseGeminiPath(r.URL.Path)
		if !ok {
			geminiError(w, http.StatusNotFound, "Not found")
			return
		}
		s.handleGemini(w, r, model, action)
	case r.URL.Path == "/v1beta/models" && r.Method == http.MethodGet:
		s.handleGeminiModels(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "Not found", 0)
	}
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// handleGeminiModels 返回 Gemini 格式模型列表
// （{"models":[{"name":"models/<id>", ...}]}），供 Gemini SDK discover 模型。
func (s *proxyServer) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	type geminiModelEntry struct {
		Name             string   `json:"name"`
		DisplayName      string   `json:"displayName"`
		SupportedActions []string `json:"supportedGenerationMethods"`
	}
	build := func(models []modelInfo) []geminiModelEntry {
		out := make([]geminiModelEntry, 0, len(models))
		for _, m := range models {
			out = append(out, geminiModelEntry{
				Name:             "models/" + m.ID,
				DisplayName:      m.ID,
				SupportedActions: []string{"generateContent", "streamGenerateContent"},
			})
		}
		return out
	}
	ar, err := s.authorize(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": build(fallbackModels)}, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": build(s.cc.models(r.Context(), ar.upstream))}, 0)
}

func (s *proxyServer) handleModels(w http.ResponseWriter, r *http.Request) {
	now := unixNow()
	build := func(models []modelInfo) modelList {
		data := make([]modelEntry, 0, len(models))
		for _, m := range models {
			data = append(data, modelEntry{ID: m.ID, Object: "model", Created: now, OwnedBy: "command-code"})
		}
		return modelList{Object: "list", Data: data}
	}

	ar, err := s.authorize(r)
	if err != nil {
		// 与旧实现一致：模型列表不强制鉴权，取不到上游 key 时回静态列表
		writeJSON(w, http.StatusOK, build(fallbackModels), 0)
		return
	}
	writeJSON(w, http.StatusOK, build(s.cc.models(r.Context(), ar.upstream)), 0)
}
