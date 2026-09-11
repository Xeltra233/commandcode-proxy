package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type logLevel int8

const (
	logDebug logLevel = iota
	logInfo
	logWarn
	logError
)

func parseLogLevel(s string) logLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return logDebug
	case "warn", "warning":
		return logWarn
	case "error":
		return logError
	default:
		return logInfo
	}
}

func logLevelName(l logLevel) string {
	switch l {
	case logDebug:
		return "debug"
	case logWarn:
		return "warn"
	case logError:
		return "error"
	default:
		return "info"
	}
}

// logger 输出 `[RFC3339ms] [level] message {json}`，与旧版 Node 实现保持同样的行格式，
// 便于既有的日志采集/告警规则直接沿用。
//
// 性能取舍：日志走一把互斥锁 + 同步写 stdout。请求级日志只在关键节点（上游报错、
// 空闲超时、客户端断连）输出，稳态下每请求 0~1 行，锁竞争不是瓶颈；真正的高频路径
// （流式 delta）不写日志。
type logger struct {
	mu   sync.Mutex
	file *os.File
	min  logLevel
}

func newLogger(level, filePath string) *logger {
	l := &logger{min: parseLogLevel(level)}
	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] cannot open log file %s: %v\n", filePath, err)
		} else {
			l.file = f
		}
	}
	return l
}

func (l *logger) log(level logLevel, msg string, kv ...any) {
	if l == nil || level < l.min {
		return
	}
	var sb strings.Builder
	sb.Grow(96 + len(msg))
	sb.WriteByte('[')
	sb.WriteString(time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	sb.WriteString("] [")
	sb.WriteString(logLevelName(level))
	sb.WriteString("] ")
	sb.WriteString(msg)
	if len(kv) >= 2 {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		if b, err := json.Marshal(m); err == nil {
			sb.WriteByte(' ')
			sb.Write(b)
		}
	}
	sb.WriteByte('\n')
	line := sb.String()

	l.mu.Lock()
	_, _ = os.Stdout.WriteString(line)
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
	l.mu.Unlock()
}

func (l *logger) Debug(msg string, kv ...any) { l.log(logDebug, msg, kv...) }
func (l *logger) Info(msg string, kv ...any)  { l.log(logInfo, msg, kv...) }
func (l *logger) Warn(msg string, kv ...any)  { l.log(logWarn, msg, kv...) }
func (l *logger) Error(msg string, kv ...any) { l.log(logError, msg, kv...) }

// prettyDuration 只用于启动横幅里的可读输出（例如 "30s"、"1m30s"）。
func prettyDuration(d time.Duration) string {
	if d <= 0 {
		return "disabled"
	}
	return d.String()
}

// prettyMs 用于展示毫秒级配置（保留旧实现的 "30000ms" 风格）。
func prettyMs(d time.Duration) string {
	if d <= 0 {
		return "disabled"
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}
