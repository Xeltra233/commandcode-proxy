package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"unicode/utf8"
)

// jsonBuf 是一个池化的 JSON/SSE 拼装缓冲。
//
// 为什么不用 json.Marshal：流式路径上每帧只有 1~3 个变量字段（id/model/文本），
// 走反射式 Marshal 会产生 4~8 次分配；这里手写字节拼装 + 池化缓冲，稳态下
// 每帧零分配。文本转义自己实现（ASCII 快路径 + 合法 UTF-8 直通），
// 非法 UTF-8 才回落标准库。
type jsonBuf struct{ b []byte }

const jsonBufInitCap = 8192

var jsonBufPool = sync.Pool{
	New: func() any { return &jsonBuf{b: make([]byte, 0, jsonBufInitCap)} },
}

func getJSONBuf() *jsonBuf {
	j := jsonBufPool.Get().(*jsonBuf)
	j.b = j.b[:0]
	return j
}

func putJSONBuf(j *jsonBuf) {
	// 异常大的缓冲不再回收（避免池常驻大内存）
	if cap(j.b) > 1<<20 {
		return
	}
	j.b = j.b[:0]
	jsonBufPool.Put(j)
}

const hexDigits = "0123456789abcdef"

func (j *jsonBuf) reset() *jsonBuf {
	j.b = j.b[:0]
	return j
}

func (j *jsonBuf) raw(s string) *jsonBuf {
	j.b = append(j.b, s...)
	return j
}

func (j *jsonBuf) byte(c byte) *jsonBuf {
	j.b = append(j.b, c)
	return j
}

func (j *jsonBuf) int(n int64) *jsonBuf {
	j.b = strconv.AppendInt(j.b, n, 10)
	return j
}

func (j *jsonBuf) uint(n uint64) *jsonBuf {
	j.b = strconv.AppendUint(j.b, n, 10)
	return j
}

func (j *jsonBuf) float(f float64) *jsonBuf {
	j.b = strconv.AppendFloat(j.b, f, 'g', -1, 64)
	return j
}

func (j *jsonBuf) bool(v bool) *jsonBuf {
	if v {
		j.b = append(j.b, "true"...)
	} else {
		j.b = append(j.b, "false"...)
	}
	return j
}

func (j *jsonBuf) null() *jsonBuf {
	j.b = append(j.b, "null"...)
	return j
}

func (j *jsonBuf) rawJSON(b []byte) *jsonBuf {
	if len(b) == 0 {
		return j.null()
	}
	j.b = append(j.b, b...)
	return j
}

// str 写入一个 JSON 字符串字面量。
func (j *jsonBuf) str(s string) *jsonBuf {
	if utf8.ValidString(s) {
		j.b = append(j.b, '"')
		start := 0
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 0x80 || (c >= 0x20 && c != '"' && c != '\\') {
				continue // 合法 UTF-8 多字节序列 或 普通 ASCII：原样输出
			}
			if start < i {
				j.b = append(j.b, s[start:i]...)
			}
			switch c {
			case '"':
				j.b = append(j.b, '\\', '"')
			case '\\':
				j.b = append(j.b, '\\', '\\')
			case '\n':
				j.b = append(j.b, '\\', 'n')
			case '\r':
				j.b = append(j.b, '\\', 'r')
			case '\t':
				j.b = append(j.b, '\\', 't')
			case '\b':
				j.b = append(j.b, '\\', 'b')
			case '\f':
				j.b = append(j.b, '\\', 'f')
			default:
				j.b = append(j.b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
			}
			start = i + 1
		}
		if start < len(s) {
			j.b = append(j.b, s[start:]...)
		}
		j.b = append(j.b, '"')
		return j
	}
	enc, _ := json.Marshal(s) // 非法 UTF-8：标准库会替换成 U+FFFD
	j.b = append(j.b, enc...)
	return j
}

func (j *jsonBuf) bytes() []byte { return j.b }

// base64StdEncode 用池化缓冲做 base64，避免每帧分配。
func base64StdEncode(raw []byte) string {
	buf := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(buf, raw)
	return string(buf)
}

// ── SSE 帧拼装 ──────────────────────────────────────

const sseDataPrefix = "data: "

// beginOpenAIChunk 写入 chat.completion.chunk 的固定前缀，delta 由调用方补齐。
func beginOpenAIChunk(j *jsonBuf, id, model string, created int64) {
	j.raw(`data: {"id":`).str(id).
		raw(`,"object":"chat.completion.chunk","created":`).int(created).
		raw(`,"model":`).str(model).
		raw(`,"choices":[{"index":0,"delta":`)
}

func endOpenAIChunk(j *jsonBuf, finishReason string) {
	j.raw(`,"finish_reason":`)
	if finishReason == "" {
		j.null()
	} else {
		j.str(finishReason)
	}
	j.raw("}]")
}

func appendOpenAIUsage(j *jsonBuf, prompt, completion, cached int64) {
	j.raw(`,"usage":{"prompt_tokens":`).int(prompt).
		raw(`,"completion_tokens":`).int(completion).
		raw(`,"total_tokens":`).int(prompt + completion).
		raw(`,"prompt_tokens_details":{"cached_tokens":`).int(cached).
		raw("}}")
}

func finishOpenAIChunk(j *jsonBuf) *jsonBuf {
	return j.raw("}\n\n")
}

// beginAnthropicEvent 写入 `event: <name>\ndata: `；调用方补 JSON 后调 endFrame。
func beginAnthropicEvent(j *jsonBuf, event string) {
	j.raw("event: ").raw(event).raw("\ndata: ")
}

// beginResponsesEvent 写入 Responses 具名事件前缀（type + sequence_number 固定在最前）。
func beginResponsesEvent(j *jsonBuf, event string, seq int64) {
	j.raw("event: ").raw(event).raw("\ndata: ").
		raw(`{"type":`).str(event).raw(`,"sequence_number":`).int(seq).raw(",")
}

func endFrame(j *jsonBuf) *jsonBuf {
	return j.raw("\n\n")
}
