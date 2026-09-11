# Command Code Proxy

> [中文文档](README_zh.md)

A reverse proxy that converts Command Code API to OpenAI / Anthropic / OpenAI-Responses compatible endpoints. Implemented in Go: a single static binary, zero third-party dependencies, pooled buffers and hand-rolled JSON framing for high-throughput SSE streaming.

**Features**: OpenAI Chat Completions + Anthropic Messages API + OpenAI Responses API | Streaming & non-streaming | Tool calling (tool_use) | Multimodal image input | Reasoning effort | Dynamic model list | Cache hit metrics | Device fingerprint disguise (per-key, auto-refresh) | **Upstream key pool** with round-robin/random rotation, failover and cooldown | **Downstream client keys** (issue keys to your users without exposing upstream keys) | `x-api-key` auth (Anthropic SDK) | Client disconnect detection with upstream abort | Zero-output → 429 auto-retry | Consecutive timeout → 429 auto-retry | Privacy-aware logging | Never crashes on a malformed `PORT` — falls back through candidate ports

**Community**: [Linux.do](https://linux.do) — a friendly Chinese tech community.

## Quick Start

Build (Go 1.23+), or use Docker:

```bash
go build -o cc-proxy .      # or: docker compose up -d
./cc-proxy                  # listens on http://0.0.0.0:3050 (config.json ships with 3050)
```

Point it at your Command Code key(s) and start calling:

```bash
CC_UPSTREAM_KEYS=user_xxxxxxxxx ./cc-proxy
```

Keys are passed per request via the `Authorization` header (or `x-api-key` for Anthropic SDKs). A key must start with `user_` (any prefix is auto-cleaned, e.g. `Bearer token_user_xxx`):

```bash
curl http://127.0.0.1:3050/v1/chat/completions   -H "Authorization: Bearer user_xxxxxxxxx"   -H "Content-Type: application/json"   -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

### Two-tier keys (optional)

By default the proxy runs in **pass-through** mode: the key your client sends is used as the upstream key. Configure `CC_CLIENT_KEYS` to switch to **distribution** mode — you hand out proxy-issued keys to clients while the proxy rotates a private pool of upstream keys:

```bash
CC_UPSTREAM_KEYS="user_aaa,user_bbb,user_ccc" CC_CLIENT_KEYS="ck_alice,ck_bob" ./cc-proxy
```

| Mode | Trigger | Incoming key | Upstream key |
|---|---|---|---|
| Pass-through | no `clientKeys` configured | any `user_` key | the client's own key (fallback: configured upstream key) |
| Distribution | `clientKeys` configured | must match a client key | taken from the upstream pool, never exposed |
| BYO blocked | distribution mode | client's `user_` key | rejected with `401` |

Upstream keys rotate `round_robin` (default) or `random`; on `401/403/429` the key is cooled down and the next one is tried (`CC_KEY_FAILOVER`).

## File Structure

```
commandcode-proxy/
├── main.go               # Entry: config wiring, routes, graceful shutdown, port fallback
├── config.go / config_test.go
├── keys.go               # Upstream key pool (rotation/failover/cooldown) + client key auth
├── cc.go / ccbuild.go    # CC upstream client, fingerprint, NDJSON, request building
├── translate.go / jsonx.go
├── server.go             # Failover orchestration, error mapping, auth
├── openai.go             # /v1/chat/completions
├── anthropic.go          # /v1/messages
├── responses.go          # /v1/responses
├── gemini.go             # /v1beta/models/{model}:generateContent
├── httpx.go / logx.go    # SSE writer, body limits, logging
├── go.mod                # Go 1.23, zero third-party dependencies
├── config.json           # Port / keys / log path etc.
├── testdata/stub_cc.py   # Stub upstream for e2e testing
├── Dockerfile            # Multi-stage Go build → alpine runtime
├── docker-compose.yml
└── .github/workflows/    # go.yml (build/test) + docker-publish.yml (GHCR)
```

## Configuration

### config.json

| Field | Default | Description |
|------|--------|-------------|
| `port` | `3000` | Listen port (repo config.json ships with `3050`) |
| `host` | `0.0.0.0` | Listen address |
| `apiBase` | `https://api.commandcode.ai` | CC API base URL |
| `projectSlug` | `""` | `x-project-slug` header (empty = per-session CLI-compatible slug) |
| `upstreamKeys` | `[]` | Upstream key pool, e.g. `[{"key":"user_xxx","name":"main"}]` |
| `clientKeys` | `[]` | Keys issued to downstream clients |
| `allowByoKeys` | auto | Allow clients to bring their own `user_` key (default: on when no `clientKeys`) |
| `keyStrategy` | `round_robin` | Upstream key rotation: `round_robin` / `random` |
| `keyFailover` | `1` | Extra upstream keys to try on 401/403/429 per request |
| `keyCooldownMs` | `60000` | Cooldown for a failed upstream key |
| `logFile` / `logLevel` | `""` / `info` | Logging |
| `useProviderModels` | `true` | Dynamically fetch model list from Provider API |
| `modelRefreshIntervalMs` | `300000` | Model list cache refresh interval (5 min) |
| `zdr` | `false` | Request ZDR-only routing from Command Code |
| `maxBodyMB` | `100` | Request body limit (413 beyond) |

### Environment Variables

| Variable | Overrides |
|----------|-----------|
| `PORT` / `CC_PORT` | `port` — tolerates `tcp://0.0.0.0:3050`, `[::]:3050`, `8080/tcp` forms; other `*_PORT` vars act as fallback candidates; never crashes |
| `HOST` | `host` |
| `CC_API_BASE` | `apiBase` |
| `CC_UPSTREAM_KEYS` / `CC_API_KEYS` | `upstreamKeys` (comma-separated) |
| `CC_CLIENT_KEYS` / `PROXY_API_KEYS` | `clientKeys` (comma-separated) |
| `CC_ALLOW_BYO` | `allowByoKeys` |
| `CC_KEY_STRATEGY` / `CC_KEY_FAILOVER` / `CC_KEY_COOLDOWN_MS` | key pool tuning |
| `CC_USE_PROVIDER_MODELS` | `useProviderModels` |
| `CC_STREAM_IDLE_MS` | Streaming upstream read idle timeout (default `30000`) |
| `CC_NONSTREAM_IDLE_MS` | Non-streaming upstream read idle timeout (default `90000`) |
| `CC_MAX_INFLIGHT` | In-process concurrent request cap (default `0` = unlimited) |
| `CC_MAX_BODY_MB` | `maxBodyMB` |
| `CMD_ZDR` | `zdr` (`1` to enable) |

When ZDR is enabled, the proxy sends `x-cmd-zdr: 1` on Command Code generation requests
and the fingerprint/lifecycle initialization requests. It does not add the header
to the npm version check or the proxy's `/provider/v1/models` catalog request.
This requests Command Code's ZDR-only routing; the upstream service remains the
authority for actual retention and provider availability.

## API Endpoints

### `POST /v1/chat/completions`

OpenAI Chat Completions compatible. Supports streaming, non-streaming, tool calling, multimodal image input, and reasoning effort.

**Request parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `model` | Yes | Model ID (see model list) |
| `messages` | Yes | Conversation messages, supports `system/user/assistant/tool` roles |
| `max_tokens` | No | Max tokens to generate (default 64000) |
| `stream` | No | SSE streaming (default false) |
| `temperature` | No | Sampling temperature (0-2) |
| `reasoning_effort` | No | Reasoning intensity: `low`/`medium`/`high`/`max` |
| `tools` | No | Tool definitions (OpenAI function calling format) |
| `tool_choice` | No | Tool selection strategy |
| `parallel_tool_calls` | No | Allow parallel tool calls |

**Simple request:**
```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [{ "role": "user", "content": "hello" }],
  "stream": true
}
```

**Multimodal image input (vision model required):**
```json
{
  "model": "xiaomi/mimo-v2.5",
  "messages": [{
    "role": "user",
    "content": [
      { "type": "text", "text": "Describe this image" },
      { "type": "image_url", "image_url": { "url": "data:image/jpeg;base64,..." } }
    ]
  }]
}
```

**Tool calling:**
```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [...],
  "tools": [{
    "type": "function",
    "function": { "name": "get_weather", "description": "...", "parameters": {...} }
  }],
  "tool_choice": "auto"
}
```

**Streaming response (SSE):**
```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking..."}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":8}}}

data: [DONE]
```

**Non-streaming response (with cache hits):**
```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "deepseek/deepseek-v4-flash",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Hello!",
      "reasoning_content": "The user said hello, I should respond."
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 7558,
    "completion_tokens": 42,
    "total_tokens": 7600,
    "prompt_tokens_details": { "cached_tokens": 7552 }
  }
}
```

### `POST /v1/messages`

Anthropic Messages API compatible endpoint. Supports streaming, non-streaming, and tool calling.

**Request body:**
```json
{
  "model": "claude-sonnet-4-6",
  "max_tokens": 1000,
  "system": "You are a helpful assistant.",
  "messages": [
    { "role": "user", "content": "hello" }
  ],
  "stream": true
}
```

**Anthropic protocol conversion (automatic):**

| Concept | Anthropic Format | Conversion |
|---------|-----------------|------------|
| System prompt | Top-level `system` field | Auto-converted to OpenAI `system` message |
| Message content | `content` array (text/tool_use/tool_result) | Auto-mapped to corresponding roles |
| Tool results | `tool_result` blocks in `user` messages | Auto-converted to `role: "tool"` |
| Tool definitions | `input_schema` | Auto-mapped to `parameters` |
| `tool_choice` | `{type:"auto"/"any"/"tool"}` | `any`→`required`, `tool`→function object |
| Reasoning | `thinking.budget_tokens` | Auto-mapped to `reasoning_effort` (≥10000→high, ≥5000→medium, ≥2000→low) |
| Stop reason | `end_turn`/`max_tokens`/`tool_use` | Auto-mapped to `stop`/`length`/`tool_calls` |
| Token usage | `input_tokens`/`output_tokens` + cache | Passed through, cache fields mapped to Anthropic format |

**Streaming response (SSE, Anthropic format):**
```
event: message_start
data: {"type":"message_start","message":{"id":"msg_xxx","type":"message","role":"assistant","content":[],"model":"...","usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10,"cache_read_input_tokens":0,"input_tokens":100}}

event: message_stop
data: {"type":"message_stop"}
```

**Non-streaming response:**
```json
{
  "id": "msg_xxx",
  "type": "message",
  "role": "assistant",
  "model": "deepseek/deepseek-v4-flash",
  "content": [{ "type": "text", "text": "Hello!" }],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 7558,
    "output_tokens": 42,
    "cache_read_input_tokens": 7552,
    "cache_creation_input_tokens": null
  }
}
```

### `POST /v1/responses`

OpenAI **Responses API** compatible (used by Codex and other Responses-native clients). Supports streaming (typed events with `sequence_number`), function calling, and reasoning summaries. `previous_response_id` / `store` are rejected with `400` — the proxy is stateless, send the full `input` each turn.

**Streaming event sequence:**
```
event: response.created
event: response.in_progress
event: response.output_item.added
event: response.content_part.added
event: response.output_text.delta
event: response.output_text.done
event: response.content_part.done
event: response.output_item.done
event: response.completed   (or response.incomplete on max_output_tokens)
```

**Usage semantics**: `input_tokens` is the total (including cached), `input_tokens_details.cached_tokens` is the cached subset — same as OpenAI.

### `POST /v1beta/models/{model}:generateContent`

Google **Gemini API** compatible. Model name is taken from the path (slashes allowed, e.g. `/v1beta/models/deepseek/deepseek-v4-flash:generateContent`). Auth via `x-goog-api-key` header or `?key=` query parameter.

- `:generateContent` — non-streaming `GenerateContentResponse`
- `:streamGenerateContent?alt=sse` — SSE stream
- `:streamGenerateContent` (no `alt=sse`) — JSON array of chunks
- `GET /v1beta/models` — Gemini-format model list

Mappings: `systemInstruction` → system message, `inlineData` → image part, `functionCall`/`functionResponse` → tool calls/results (paired by name), `toolConfig.functionCallingConfig.mode` (`AUTO`/`ANY`/`NONE`) → `tool_choice`, `generationConfig.thinkingConfig.thinkingBudget` → reasoning effort. Thought summaries are returned as `parts[].thought: true`.

### `GET /v1/models`

Returns available model list. Fetched dynamically from Provider API (5 min cache), falls back to hardcoded list on failure.

### `GET /health`

Health check. Returns `OK`.

## Error Codes

| HTTP Status | Description |
|-------------|-------------|
| 400 | Invalid request format |
| 401 | API Key missing / invalid format / rejected (Key must start with `user_`; sent via `Authorization: Bearer` or `x-api-key`) |
| 429 | Zero output tokens, or idle timeout (30s streaming / 90s non-streaming) — SDK auto-retry with `Retry-After`; after 3 consecutive timeouts a "reduce context" hint is returned |
| 502 | CC upstream error |

## Model List

The proxy returns a live model list via `GET /v1/models`. Below are common models for reference; the actual list depends on the live API response — see [Command Code Pricing](https://commandcode.ai/docs/resources/pricing-limits) for plan details.

### Common Models

| Model ID | Provider |
|----------|----------|
| `claude-sonnet-4-6` / `claude-opus-4-8` / `claude-opus-4-7` / `claude-haiku-4-5-20251001` | Anthropic |
| `gpt-5.5` / `gpt-5.4` / `gpt-5.4-mini` / `gpt-5.3-codex` | OpenAI |
| `deepseek/deepseek-v4-pro` / `deepseek/deepseek-v4-flash` | DeepSeek |
| `moonshotai/Kimi-K2.6` / `moonshotai/Kimi-K2.5` | Kimi |
| `zai-org/GLM-5.1` / `zai-org/GLM-5` | GLM |
| `MiniMaxAI/MiniMax-M3` / `MiniMaxAI/MiniMax-M2.7` / `MiniMaxAI/MiniMax-M2.5` | MiniMax |
| `Qwen/Qwen3.7-Max` / `Qwen/Qwen3.6-Max-Preview` / `Qwen/Qwen3.6-Plus` | Qwen |
| `stepfun/Step-3.7-Flash` / `stepfun/Step-3.5-Flash` | Step |
| `xiaomi/mimo-v2.5-pro` / `xiaomi/mimo-v2.5` | Xiaomi (**image input supported**) |
| `google/gemini-3.5-flash` / `google/gemini-3.1-flash-lite` | Gemini |

> ⚠️ Some models (e.g. `deepseek-v4-flash`, `claude-sonnet-4-6`) do not support image input. Use `xiaomi/mimo-v2.5`, `Kimi-K2.5`, or other vision models for multimodal.

## Integration Examples

### Python (OpenAI SDK)
```python
from openai import OpenAI

client = OpenAI(
    api_key="user_xxxxxxxxx",
    base_url="http://127.0.0.1:3050/v1",
)

response = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "hello"}],
    stream=True,
)
for chunk in response:
    print(chunk.choices[0].delta.content or "", end="")
```

### cURL
```bash
curl http://127.0.0.1:3050/v1/chat/completions \
  -H "Authorization: Bearer user_xxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### Cursor
Add a Custom Provider in Cursor settings:
- **API Base URL**: `http://127.0.0.1:3050/v1`
- **API Key**: `user_xxxxxxxxx`
- **Model**: Choose from the model list

### Anthropic (Python SDK)
```python
import anthropic

client = anthropic.Anthropic(
    api_key="user_xxxxxxxxx",
    base_url="http://127.0.0.1:3050",
)
message = client.messages.create(
    model="deepseek/deepseek-v4-flash",
    max_tokens=1000,
    system="You are helpful.",
    messages=[{"role": "user", "content": "hello"}],
)
print(message.content[0].text)
```

The Anthropic SDK authenticates via the `x-api-key` header — supported by the proxy natively (no `Authorization` header needed).

### OpenCode
```json
{
  "provider": "openai-compatible",
  "baseUrl": "http://127.0.0.1:3050/v1",
  "apiKey": "user_xxxxxxxxx"
}
```

## Anti-Detection

Based on analysis of official CLI traffic (version auto-fetched from npm registry):

| Mechanism | Implementation |
|-----------|---------------|
| **Device Fingerprint** | `POST /alpha/fingerprint/record` before first request per key; random fingerprint pool (15 CPUs, global timezones), SHA-256 hashed, per-key binding, refreshed every 8h + 2h jitter |
| **Lifecycle Events** | `POST /alpha/lifecycle-events` (`cli_session_exists`) sent in parallel with fingerprint on session init |
| **Per-Key Session** | One session per API key, 12h expiry + 1h random jitter |
| **Version** | `x-command-code-version` auto-fetched from npm registry (24h refresh) |
| **CLI Envelope** | config/memory/taste/skills/permissionMode/params |
| **OpenTelemetry** | `traceparent` (W3C Trace Context) |
| **Environment** | `x-cli-environment: production`, `x-co-flag: "false"`, `x-taste-learning: "false"` |
| **Project Slug** | `x-project-slug` generated from session ID (CLI-compatible format) |
| **Reasoning Effort** | `reasoning_effort` pass-through (low/medium/high/max) |
| **Key Validation** | Regex `user_[a-zA-Z0-9_-]+` on `Authorization: Bearer`, `x-api-key`, `x-goog-api-key` or `?key=`, auto-cleans extra paths/prefixes, rejects `sk-xxx` format |
| **Stream Timeout** | 30s streaming / 90s non-streaming → 429 with SDK auto-retry |
| **Consecutive Timeout** | 3 consecutive timeouts before "reduce context" hint |
| **Zero-Output Guard** | outputTokens=0 → 429 `rate_limit_error` (SDK auto-retry, anti false billing) |
| **Upstream Abort** | Go request-context cancellation: client disconnect or any error path aborts the upstream request |
| **Privacy Logging** | No API key fragments, no error bodies, no stack traces in logs |

## Protocol Details

### CC API Request Structure

```json
{
  "config": {
    "workingDir": "C:\\project",
    "date": "2026-06-07",
    "environment": "win32-x64, Node.js v24.16.0",
    "structure": [],
    "isGitRepo": false,
    "currentBranch": "",
    "mainBranch": "",
    "gitStatus": "",
    "recentCommits": []
  },
  "memory": null,
  "taste": null,
  "skills": "",
  "permissionMode": "standard",
  "params": {
    "model": "deepseek/deepseek-v4-flash",
    "messages": [...],
    "max_tokens": 64000,
    "stream": true,
    "reasoning_effort": "max"
  }
}
```

Conditional fields: `system` (extracted from `system` messages), `temperature`, `reasoning_effort`, `tools` (mapped to CC `input_schema` format).

### CC API Image Message Format

The CLI sends images in this format:

```json
{
  "role": "user",
  "content": [
    { "type": "image", "image": "data:image/jpeg;base64,..." },
    { "type": "text", "text": "What does this image say?" }
  ]
}
```

The proxy receives OpenAI `image_url` format and converts it to the above CC format transparently.

## Docker Deployment

### Pull from GHCR

Pre-built multi-arch images (`linux/amd64` + `linux/arm64`) are published to the GitHub Container Registry automatically on every `v*` tag via GitHub Actions:

```bash
docker pull ghcr.io/maxeaglet/commandcode-proxy:latest
docker run -d --name cc-proxy -p 3050:3050   -e PORT=3050   -e CC_UPSTREAM_KEYS=user_xxxxxxxxx   ghcr.io/maxeaglet/commandcode-proxy:latest
```

The `latest` tag is updated on each release. The image is public — no login required to pull.

### Quick Start (docker compose)

```bash
docker compose up -d
```

The proxy will listen on `http://0.0.0.0:3050`. Set `PROXY_PORT` to customize the host port:

```bash
PROXY_PORT=13050 docker compose up -d
```

### Build from Source

```bash
docker build -t commandcode-proxy:latest .
docker run -d -p 3050:3050 -e PORT=3050 -e CC_UPSTREAM_KEYS=user_xxxxxxxxx commandcode-proxy:latest
```

### Multi-Architecture Build

The publish workflow builds `linux/amd64` + `linux/arm64` automatically on `v*` tags. To build locally:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t commandcode-proxy:latest .
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3050` | Container listen port |
| `PROXY_PORT` | `3050` | Host port (compose only) |
| `CC_MAX_BODY_MB` | `100` | Max request body size in MB; oversized requests are rejected with `HTTP 413` |
| `CC_CLIENT_DRAIN_TIMEOUT_MS` | *(unset = disabled)* | Drop the client and abort upstream when downstream backpressure blocks longer than this; see [Stalled clients](#stalled-clients-neither-reading-nor-disconnecting) |
| `CC_STREAM_IDLE_MS` | `30000` | Streaming upstream read idle timeout in ms; see [Upstream idle timeouts](#upstream-idle-timeouts) |
| `CC_NONSTREAM_IDLE_MS` | `90000` | Non-streaming upstream read idle timeout in ms |
| `CC_MAX_INFLIGHT` | `0` (unlimited) | In-process request cap; over-limit returns `503` + `Retry-After`; see [In-flight cap](#in-flight-cap-optional) |

## In-flight Cap (Optional)

**Off by default** (`CC_MAX_INFLIGHT` unset = no concurrency limit), so existing behaviour is unchanged.

This project is a **pure proxy layer**; concurrency control belongs downstream — use your reverse proxy for per-IP / per-key limits (see the `limit_conn` block in [Memory & Deployment](#memory--deployment)). This option is **not** a replacement for that; it only covers running **without** a reverse proxy with an in-process, **global-only** guard:

```bash
CC_MAX_INFLIGHT=32 ./cc-proxy    # at most 32 concurrent requests
```

Over the limit it returns `503` + `Retry-After: 5` + `type: server_busy` — a shape the official OpenAI / Anthropic SDKs retry with backoff, instead of the client seeing a connection reset. `/health` and `/` are exempt so liveness probes and orchestrators never receive a 503 because business traffic is busy.

**Why it exists**: memory is `in-flight × (0.13 MB + 5.5 × body_MB)`. `CC_MAX_BODY_MB` bounds only the **per-request** term; nothing bounds the multiplier — at the default 100 MB, N concurrent requests can cost N × 550 MB.

> ⚠️ Enabling this is **not** the same as being memory-safe: 32 × 550 MB still exceeds a small box. For a hard bound, lower `CC_MAX_BODY_MB` **as well**.

## Upstream Idle Timeouts

Two upstream read idle watchdogs; on expiry the proxy returns `429` (with `retry_after`) so the SDK retries automatically:

| Env var | Default | Applies to |
|---|---|---|
| `CC_STREAM_IDLE_MS` | `30000` | Streaming requests |
| `CC_NONSTREAM_IDLE_MS` | `90000` | Non-streaming requests |

**Semantics**: they measure only the time spent waiting inside `reader.read()`, reset on every received chunk — **not the total request duration**. As long as upstream keeps emitting, the watchdog never fires, even for a request that has been running for tens of minutes.

**The defaults differ from the official CLI, and that is a known trade-off** ([#19](https://github.com/MAXeaglet/commandcode-proxy/issues/19)): the official CLI has **no** upstream idle timeout at all — deobfuscating `command-code@1.50.0` shows every `createApiClient({ baseUrl })` call site passes no `timeout`, and 700+ second stalls complete successfully. This proxy keeps 30 s to catch genuinely dead connections; the cost is that a reasoning model's long prefill/first-token stall can be killed.

If you see `429 Response timeout` or `zero output tokens` where the log shows `elapsedMs ≈ 30000` and `bytesReceived = 0`, the watchdog killed a healthy stall — raise it:

```bash
CC_STREAM_IDLE_MS=300000 ./cc-proxy      # 5 minutes
```

> ⚠️ A false kill costs more than one failed request: the abort returns `429 + retry_after`, the SDK retries automatically, and a retry **resends the entire context** — so each false kill re-pays the full prefill on long conversations.

## Memory & Deployment

The Go implementation keeps at most ~2 copies of a request body (raw bytes + parsed structure) instead of the ~5–7 copies a Node string/Buffer pipeline produces, and streams all four protocol translations without buffering whole responses. Streaming backpressure propagates upstream: when the client stops reading, the SSE writer stops, the upstream read stalls, and the idle watchdog eventually closes the connection — no unbounded response buffering.

Still, the same deployment rules apply:

### Request body limit

Requests larger than `CC_MAX_BODY_MB` (default **100 MB**) are rejected with `HTTP 413` before being parsed. The limit is **per request**; a public deployment should also bound concurrency at the reverse proxy.

### Suggested nginx front

```nginx
map $http_authorization $cc_key { default $http_authorization; "" $http_x_api_key; }
map "" $cc_global_key { default "global"; }

limit_conn_zone $binary_remote_addr zone=cc_ip:10m;
limit_conn_zone $cc_key             zone=cc_key:10m;
limit_conn_zone $cc_global_key      zone=cc_global:10m;

location /v1/ {
    client_max_body_size 4m;   # must be <= CC_MAX_BODY_MB
    limit_conn cc_ip     8;
    limit_conn cc_key    4;
    limit_conn cc_global 32;   # this *is* the memory ceiling
    limit_conn_status 429;
    proxy_pass http://127.0.0.1:3050;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_read_timeout 300s;   # must exceed the 30s stream idle timeout
}
```

### Stalled clients (neither reading nor disconnecting)

A client that neither reads nor disconnects holds its upstream connection. Opt in to a write deadline that drops such clients (a client making any drain progress never triggers it):

```bash
CC_CLIENT_STALL_MS=60000 ./cc-proxy
```

A more robust cap still belongs at the reverse proxy (`limit_conn`), since only it knows how much concurrency a given deployment can afford.

### Other notes

- **Log file**: prefer leaving `logFile` empty and collecting stdout; the logger writes to stdout asynchronously through the OS pipe buffer.
- **systemd guard rails**: set `MemoryMax=` so an overshoot kills the proxy, not `sshd`/`nginx`.
- **Multi-account + multiple instances**: per-key session and fingerprint state lives in process memory, so the same API key served by two instances gets two different sessions and **two different device fingerprints** — upstream sees one account on multiple machines. Scale with consistent hashing on the API key (`hash $cc_key consistent`), not round-robin.

## Disclaimer

This project is for **educational and research purposes** only.

- **Unofficial**: This project is not affiliated with Command Code in any way.
- **Personal Use**: Users assume all responsibility. Please comply with the [Command Code Terms of Service](https://commandcode.ai/tos).
- **API Key**: This project does not collect, upload, or leak your API Key. The key is sent per request via the `Authorization: Bearer <key>` or `x-api-key` header and is never logged; an optional `apiKey` field in `config.json` serves only as a local fallback and never leaves your machine.
- **Compliance**: The protocol is based on passive observation of local CLI network traffic. No unauthorized access, cracking, or tampering of the server has been performed.
- **Account Risk**: Keep usage frequency consistent with normal CLI usage. Extremely high concurrent calls may trigger risk controls.

---

## Development

```bash
go vet ./...
go test -race ./...
go build -o cc-proxy .
python testdata/stub_cc.py 9701 &     # stub CC upstream
CC_API_BASE=http://127.0.0.1:9701 CC_UPSTREAM_KEYS=user_test ./cc-proxy
```
