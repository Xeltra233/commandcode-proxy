# Command Code Proxy

> [English Docs](README.md)

将 Command Code API 转换为 OpenAI / Anthropic / OpenAI-Responses / Gemini 兼容接口的反代代理。Go 实现：单个静态二进制，零第三方依赖，池化缓冲与手写 JSON 帧拼装，面向高吞吐 SSE 流式场景。

**完整功能**：OpenAI Chat Completions + Anthropic Messages API + OpenAI Responses API + Google Gemini API | 流式/非流式输出 | 工具调用 (tool_use) | 多模态图片输入 | 推理强度 (reasoning_effort) | 动态模型列表 | 缓存命中指标 | 设备指纹伪装（per-key 绑定、自动刷新）| **上游 key 池**（round_robin/random 轮换、失败冷却、自动故障转移）| **下游分发 key**（给客户端发 key，不暴露上游 key）| `x-api-key` / `x-goog-api-key` 鉴权 | 客户端断连检测（上游中止）| 零输出 → 429 自动重试 | 连续超时 → 429 自动重试 | 隐私保护日志 | 非法 `PORT` 不崩溃——按候选端口兜底

**社区**: [Linux.do](https://linux.do) — 一个友好的中文技术社区。

## 快速开始

编译（Go 1.23+）或直接用 Docker：

```bash
go build -o cc-proxy .      # 或：docker compose up -d
./cc-proxy                  # 监听 http://0.0.0.0:3050（仓库自带 config.json）
```

配置上游 key 并启动：

```bash
CC_UPSTREAM_KEYS=user_xxxxxxxxx ./cc-proxy
```

Key 通过 `Authorization` 请求头（Anthropic SDK 可用 `x-api-key`，Gemini SDK 可用 `x-goog-api-key` 或 `?key=`）传入。Key 必须以 `user_` 开头（自动匹配任意前缀，如 `Bearer token_user_xxx`）：

```bash
curl http://127.0.0.1:3050/v1/chat/completions   -H "Authorization: Bearer user_xxxxxxxxx"   -H "Content-Type: application/json"   -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

### 双层 key（可选）

默认是**透传模式**：客户端带的 key 直接当上游 key 用。配置 `CC_CLIENT_KEYS` 即切换为**分发模式**——向下游发放代理自签的 key，上游 key 池私有轮换：

```bash
CC_UPSTREAM_KEYS="user_aaa,user_bbb,user_ccc" CC_CLIENT_KEYS="ck_alice,ck_bob" ./cc-proxy
```

| 模式 | 触发条件 | 入站 key | 上游 key |
|---|---|---|---|
| 透传 | 未配置 `clientKeys` | 任意 `user_` key | 客户端自带（回退：配置的上游 key） |
| 分发 | 配置了 `clientKeys` | 必须匹配某个 client key | 从上游池轮换取用，永不暴露 |
| BYO 关闭 | 分发模式 | 客户端自带 `user_` key | `401` 拒绝 |

上游 key 按 `round_robin`（默认）或 `random` 轮换；遇 `401/403/429` 冷却该 key 并换下一个（`CC_KEY_FAILOVER`）。

## 文件结构

```
commandcode-proxy/
├── main.go               # 入口：配置装配、路由、优雅退出、端口兜底
├── config.go / config_test.go
├── keys.go               # 上游 key 池（轮换/故障转移/冷却）+ 客户端 key 鉴权
├── cc.go / ccbuild.go    # CC 上游客户端、指纹、NDJSON、请求体构建
├── translate.go / jsonx.go
├── server.go             # 故障转移编排、错误映射、鉴权
├── openai.go             # /v1/chat/completions
├── anthropic.go          # /v1/messages
├── responses.go          # /v1/responses
├── gemini.go             # /v1beta/models/{model}:generateContent
├── httpx.go / logx.go    # SSE 写出、请求体上限、日志
├── go.mod                # Go 1.23，零第三方依赖
├── config.json           # 端口 / key / 日志等
├── testdata/stub_cc.py   # e2e 测试用 CC 上游桩
├── Dockerfile            # 多阶段 Go 构建 → alpine 运行时
├── docker-compose.yml
└── .github/workflows/    # go.yml（构建/测试）+ docker-publish.yml（GHCR）
```

## 配置

### config.json

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `port` | `3000` | 监听端口（仓库自带 config.json 为 3050） |
| `host` | `0.0.0.0` | 监听地址 |
| `apiBase` | `https://api.commandcode.ai` | CC API 地址 |
| `projectSlug` | `""` | `x-project-slug` header（空 = 按会话生成 CLI 兼容 slug） |
| `upstreamKeys` | `[]` | 上游 key 池，如 `[{"key":"user_xxx","name":"main"}]` |
| `clientKeys` | `[]` | 向下游分发的客户端 key |
| `allowByoKeys` | 自动 | 允许客户端自带 `user_` key（默认：未配置 `clientKeys` 时开启） |
| `keyStrategy` | `round_robin` | 上游 key 轮换：`round_robin` / `random` |
| `keyFailover` | `1` | 单请求遇 401/403/429 时额外尝试的上游 key 数 |
| `keyCooldownMs` | `60000` | 失败上游 key 的冷却时长 |
| `logFile` / `logLevel` | `""` / `info` | 日志 |
| `useProviderModels` | `true` | 从 Provider API 动态拉取模型列表 |
| `modelRefreshIntervalMs` | `300000` | 模型列表缓存刷新间隔（5min） |
| `zdr` | `false` | 请求 Command Code 使用 ZDR-only 路由 |
| `plan` | `""` | 套餐等级覆盖（`go`/`goat`/`pro`/`max`）；空 = 从 billing 接口自动探测 |
| `planPremiumModels` | `[]` | 受限套餐下显式放行的 premium 模型 |
| `maxBodyMB` | `100` | 请求体上限（超出 413） |

### 环境变量

| 变量 | 对应 config 字段 |
|------|-----------------|
| `PORT` / `CC_PORT` | `port` —— 容忍 `tcp://0.0.0.0:3050`、`[::]:3050`、`8080/tcp` 等写法；其它 `*_PORT` 变量作为兜底候选；永不崩溃 |
| `HOST` | `host` |
| `CC_API_BASE` | `apiBase` |
| `CC_UPSTREAM_KEYS` / `CC_API_KEYS` | `upstreamKeys`（逗号分隔） |
| `CC_CLIENT_KEYS` / `PROXY_API_KEYS` | `clientKeys`（逗号分隔） |
| `CC_ALLOW_BYO` | `allowByoKeys` |
| `CC_KEY_STRATEGY` / `CC_KEY_FAILOVER` / `CC_KEY_COOLDOWN_MS` | key 池调优 |
| `CC_USE_PROVIDER_MODELS` | `useProviderModels` |
| `CC_STREAM_IDLE_MS` | 流式上游读空闲超时（默认 `30000`）|
| `CC_NONSTREAM_IDLE_MS` | 非流式上游读空闲超时（默认 `90000`）|
| `CC_MAX_INFLIGHT` | 进程内在途请求上限（默认 `0` = 不限）|
| `CC_MAX_BODY_MB` | `maxBodyMB` |
| `CMD_ZDR` | `zdr`（`1` 开启） |
| `CC_PLAN` | `plan` —— 手动覆盖；未设置时自动探测 |
| `CC_PLAN_PREMIUM_MODELS` | `planPremiumModels`（逗号分隔） |
| `CC_UPSTREAM_PROXY` | `upstreamProxy` —— 上游出口代理（http/https/socks5）；部署机 IP 被上游 bot 规则拦截时使用 |
| `upstreamProxy` | `""` | 同上，config.json 写法 |


开启 ZDR 后，代理会在 Command Code 生成请求以及 fingerprint/lifecycle 初始化请求中附加
`x-cmd-zdr: 1`。npm 版本检查和代理自己的 `/provider/v1/models` 模型目录请求不会附加该
header。该开关只是请求 Command Code 使用 ZDR-only 路由，实际数据留存和上游可用性仍由上游服务决定。

## API 接口

### `POST /v1/chat/completions`

OpenAI Chat Completions 兼容。支持流式和非流式、工具调用、多模态图片输入、推理强度。

**请求体参数：**

| 参数 | 必填 | 说明 |
|------|------|------|
| `model` | 是 | 模型 ID（见模型列表） |
| `messages` | 是 | 对话消息，支持 `system/user/assistant/tool` 角色 |
| `max_tokens` | 否 | 最大生成 token（默认 64000） |
| `stream` | 否 | 是否 SSE 流式（默认 false） |
| `temperature` | 否 | 采样温度（0-2）|
| `reasoning_effort` | 否 | 推理强度 `low`/`medium`/`high`/`max` |
| `tools` | 否 | 工具定义（OpenAI function calling 格式）|
| `tool_choice` | 否 | 工具选择策略 |
| `parallel_tool_calls` | 否 | 是否允许并行工具调用 |

**简单请求：**
```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [{ "role": "user", "content": "hello" }],
  "stream": true
}
```

**多模态图片输入（需 vision 模型）：**
```json
{
  "model": "xiaomi/mimo-v2.5",
  "messages": [{
    "role": "user",
    "content": [
      { "type": "text", "text": "描述这张图片" },
      { "type": "image_url", "image_url": { "url": "data:image/jpeg;base64,..." } }
    ]
  }]
}
```

**工具调用：**
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

**流式响应（SSE）：**
```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考过程"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":8}}}

data: [DONE]
```

**非流式响应（含缓存命中）：**
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

Anthropic Messages API 兼容端点。支持流式和非流式、工具调用。

**请求体：**
```json
{
  "model": "claude-sonnet-4-6",
  "max_tokens": 1000,
  "system": "你是一个有用的助手。",
  "messages": [
    { "role": "user", "content": "hello" }
  ],
  "stream": true
}
```

**Anthropic 协议差异（自动转换）：**

| 概念 | Anthropic 原始格式 | 转换说明 |
|------|-------------------|----------|
| System prompt | 顶层 `system` 字段 | 自动转为 OpenAI `system` message |
| 消息内容 | `content` 数组（text/tool_use/tool_result） | 自动映射为对应角色 |
| 工具结果 | `user` 消息中的 `tool_result` 块 | 自动转为 `role: "tool"` |
| 工具定义 | `input_schema` | 自动映射为 `parameters` |
| `tool_choice` | `{type:"auto"/"any"/"tool"}` | `any`→`required`，`tool`→function 对象 |
| 推理强度 | `thinking.budget_tokens` | 自动映射为 `reasoning_effort`（≥10000→high, ≥5000→medium, ≥2000→low） |
| 停止原因 | `end_turn`/`max_tokens`/`tool_use` | 自动映射为 `stop`/`length`/`tool_calls` |
| Token 用量 | `input_tokens`/`output_tokens` + 缓存 | 透传，缓存字段映射为 Anthropic 格式 |

**流式响应（SSE，Anthropic 格式）：**
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

**非流式响应：**
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

OpenAI **Responses API** 兼容（Codex 等 Responses 原生客户端）。支持流式（带 `sequence_number` 的具名事件）、函数调用与 reasoning 摘要。`previous_response_id` / `store` 返回 `400` —— 代理无状态，每轮请发完整 `input`。

**流式事件序列：**
```
event: response.created
event: response.in_progress
event: response.output_item.added
event: response.content_part.added
event: response.output_text.delta
event: response.output_text.done
event: response.content_part.done
event: response.output_item.done
event: response.completed   （或 max_output_tokens 触发 response.incomplete）
```

**usage 语义**：`input_tokens` 为总数（含缓存），`input_tokens_details.cached_tokens` 是其中缓存部分 —— 与 OpenAI 语义一致。

### `POST /v1beta/models/{model}:generateContent`

Google **Gemini API** 兼容。模型名从路径取（可含斜杠，如 `/v1beta/models/deepseek/deepseek-v4-flash:generateContent`）。鉴权用 `x-goog-api-key` 头或 `?key=` 查询参数。

- `:generateContent` —— 非流式 `GenerateContentResponse`
- `:streamGenerateContent?alt=sse` —— SSE 流
- `:streamGenerateContent`（无 `alt=sse`）—— JSON 数组
- `GET /v1beta/models` —— Gemini 格式模型列表

映射：`systemInstruction` → system 消息、`inlineData` → 图片 part、`functionCall`/`functionResponse` → 工具调用/结果（按 name 配对）、`toolConfig.functionCallingConfig.mode`（`AUTO`/`ANY`/`NONE`）→ `tool_choice`、`generationConfig.thinkingConfig.thinkingBudget` → 推理强度。思考摘要以 `parts[].thought: true` 返回。

### `GET /v1/models`

返回可用模型列表。优先从 Provider API 动态拉取（5min 缓存），失败回退硬编码列表。

### `GET /health`

健康检查。返回 `OK`。

## 错误码

| HTTP 状态 | 说明 |
|-----------|------|
| 400 | 请求格式错误 |
| 401 | API Key 缺失/格式不对/无效（Key 必须以 `user_` 开头；通过 `Authorization: Bearer`、`x-api-key`、`x-goog-api-key` 或 `?key=` 传入） |
| 429 | 零输出 token，或流空闲超时（30s 流式 / 90s 非流式）——带 `Retry-After`，SDK 自动重试；连续 3 次超时返回"压缩上下文"提示 |
| 502 | CC 上游错误 |

## 套餐与模型可用性

上游模型目录（`GET /v1/models`）返回的是**全量**模型，与订阅套餐无关——列表里出现 Claude Opus 不代表你的套餐能调它。代理分三层解决：

1. **自动探测（默认）**——用上游 key 调官方 CLI 同款 billing 接口（`/alpha/whoami` → `/alpha/billing/credits` + `/alpha/billing/subscriptions`），拿到账号的 `planId`（`individual-go` / `individual-goat` / `individual-pro` / `individual-max` …）与额度，按官方访问评估逻辑过滤模型列表。结果缓存 30 分钟（失败 2 分钟）。充值/免费额度全解锁，与官方行为一致。
2. **静态回退**——探测失败时可用内置套餐表（从官方 CLI 还原，含每套餐模型黑名单，如 Pro 下的 Opus）强制指定：`CC_PLAN=go|goat|pro|max`。
3. **运行时自适应**——上游仍以套餐原因拒绝某模型时，自动把该模型从列表剔除 1 小时。

最终效果：代理返回的模型列表只包含当前账号真正能调的模型。

### 反爬伪装

代理逐字节复刻官方 CLI 的完整请求签名：

- **请求头**——`User-Agent: cli`、`x-cli-environment`、`x-command-code-version`（实时从 npm 拉取）、`x-project-slug`、`x-taste-learning`、`x-session-id`（每 key 独立，12h±1h 轮换，与真实 CLI 会话一致）、`x-cmd-zdr`、W3C `traceparent`。
- **设备指纹**——`/alpha/fingerprint/record` 载荷严格按官方公式（`sha256("command-code:device-fingerprint:v1" + "\0machine\0" + …)`），组件高度拟真：platform 联动的 OS 版本 / 主机名 / machine-id 格式、darwin 配 Apple Silicon CPU 型号、CPU/内存/时区组合合理、2~5 张网卡哈希。每 key 固定一枚指纹，按官方"首次 + 8h±2h"节奏重新注册，并同步上报 `/alpha/lifecycle-events`（`cli_session_exists`）。

若部署机 IP 仍被 bot 规则挑战（数据中心 IP 很常见），设置 `CC_UPSTREAM_PROXY` 让上游流量走干净出口。

Go 套餐示例（自动探测）：仅开源模型——`deepseek/*`、`qwen/*`、`glm-*`、`kimi-*`、`minimax-*`、`mimo-*` 及免费模型。Premium（Claude/GPT/Gemini 系）需 Pro 及以上；Pro 下 Opus / GPT-6-Astra / Fugu-Utra 一档仍被排除。按量充值（`/extra`）全解锁。

## 模型列表

代理访问 `GET /v1/models` 会返回实时模型列表。以下为常见模型参考，完整列表以实际接口返回为准——各模型套餐可参考 [Command Code Pricing](https://commandcode.ai/docs/resources/pricing-limits)。

### 常用模型

| 模型 ID | 提供商 |
|---------|--------|
| `claude-sonnet-4-6` / `claude-opus-4-8` / `claude-opus-4-7` / `claude-haiku-4-5-20251001` | Anthropic |
| `gpt-5.5` / `gpt-5.4` / `gpt-5.4-mini` / `gpt-5.3-codex` | OpenAI |
| `deepseek/deepseek-v4-pro` / `deepseek/deepseek-v4-flash` | DeepSeek |
| `moonshotai/Kimi-K2.6` / `moonshotai/Kimi-K2.5` | Kimi |
| `zai-org/GLM-5.1` / `zai-org/GLM-5` | GLM |
| `MiniMaxAI/MiniMax-M3` / `MiniMaxAI/MiniMax-M2.7` / `MiniMaxAI/MiniMax-M2.5` | MiniMax |
| `Qwen/Qwen3.7-Max` / `Qwen/Qwen3.6-Max-Preview` / `Qwen/Qwen3.6-Plus` | Qwen |
| `stepfun/Step-3.7-Flash` / `stepfun/Step-3.5-Flash` | Step |
| `xiaomi/mimo-v2.5-pro` / `xiaomi/mimo-v2.5` | Xiaomi（**支持图片输入**） |
| `google/gemini-3.5-flash` / `google/gemini-3.1-flash-lite` | Gemini |

> ⚠️ 部分模型（如 `deepseek-v4-flash`、`claude-sonnet-4-6`）不支持图片输入。如需多模态请用 `xiaomi/mimo-v2.5`、`Kimi-K2.5` 等 vision 模型。

## 接入示例

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
在 Cursor 设置中添加 Custom Provider：
- **API Base URL**: `http://127.0.0.1:3050/v1`
- **API Key**: `user_xxxxxxxxx`
- **Model**: 从模型列表中选择

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

Anthropic SDK 通过 `x-api-key` 头鉴权——代理已原生支持（无需 `Authorization` 头）。

### OpenCode
```json
{
  "provider": "openai-compatible",
  "baseUrl": "http://127.0.0.1:3050/v1",
  "apiKey": "user_xxxxxxxxx"
}
```

## 反检测

基于对官方 CLI 网络流量的分析（版本号从 npm registry 动态拉取），实现了以下兼容适配：

| 机制 | 实现 |
|------|------|
| **设备指纹** | 每个 Key 首次请求前发送 `POST /alpha/fingerprint/record`；随机指纹池（15 种 CPU、全球时区）、SHA-256 哈希、per-key 绑定，每 8h+2h 抖动刷新 |
| **生命周期声明** | 会话初始化时与指纹并行发送 `POST /alpha/lifecycle-events`（`cli_session_exists`） |
| **按 Key 分 Session** | 每个 API Key 独立 session，12h 过期 + 1h 随机抖动 |
| **动态版本号** | `x-command-code-version` 从 npm registry 自动拉取（24h 刷新） |
| **CLI 信封格式** | config/memory/taste/skills/permissionMode/params |
| **OpenTelemetry** | `traceparent` (W3C Trace Context) |
| **环境标识** | `x-cli-environment: production`、`x-co-flag: "false"`、`x-taste-learning: "false"` |
| **Project Slug** | 从 sessionId 生成的 `x-project-slug`（与真实 CLI 格式一致） |
| **思考强度** | `reasoning_effort` 透传 (low/medium/high/max) |
| **API Key 格式验证** | 对 `Authorization: Bearer` 或 `x-api-key` 用正则 `user_[a-zA-Z0-9_-]+` 提取，自动清理多余路径/前缀，`sk-xxx` 等非 `user_` 格式拒 |
| **流式超时保护** | 流式 30s、非流式 90s → 429 + SDK 自动重试 |
| **连续超时阈值** | 连续 3 次超时后才提示压缩上下文 |
| **零输出防护** | outputTokens=0 → 429 `rate_limit_error`（SDK 自动重试，反异常计费） |
| **上游中止** | Go 请求上下文取消：客户端断连或任意错误路径立即中止上游请求 |
| **隐私保护日志** | 日志不含 API Key 片段、错误 body、stack trace |

## 协议细节

### CC API 请求体结构

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

条件字段：`system`（从 system 消息提取）、`temperature`、`reasoning_effort`、`tools`（映射为 CC `input_schema` 格式）。

### CC API 图片消息格式

CLI 发送图片的格式：

```json
{
  "role": "user",
  "content": [
    { "type": "image", "image": "data:image/jpeg;base64,..." },
    { "type": "text", "text": "图里写了什么" }
  ]
}
```

代理收到 OpenAI `image_url` 格式后自动转为上述 CC 格式透传。

## Docker 部署

### 从 GHCR 拉取

每次打 `v*` tag 时 GitHub Actions 会自动构建并推送多架构镜像（`linux/amd64` + `linux/arm64`）到 GitHub Container Registry：

```bash
docker pull ghcr.io/maxeaglet/commandcode-proxy:latest
docker run -d --name cc-proxy -p 3050:3050 \n  -e PORT=3050 \n  -e CC_UPSTREAM_KEYS=user_xxxxxxxxx \n  ghcr.io/maxeaglet/commandcode-proxy:latest
```

每次发版都会更新 `latest` 标签。镜像为公共可见，拉取无需登录。

### 快速启动 (docker compose)

```bash
docker compose up -d
```

代理将在 `http://0.0.0.0:3050` 监听。通过 `PROXY_PORT` 自定义主机端口：

```bash
PROXY_PORT=13050 docker compose up -d
```

### 从源码构建

```bash
docker build -t commandcode-proxy:latest .
docker run -d -p 3050:3050 -e PORT=3050 -e CC_UPSTREAM_KEYS=user_xxxxxxxxx commandcode-proxy:latest
```

### 多架构构建

发布工作流会在 `v*` tag 上自动构建 `linux/amd64` + `linux/arm64`。本地构建：

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t commandcode-proxy:latest .
```

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `3050` | 容器内监听端口 |
| `PROXY_PORT` | `3050` | 主机映射端口（仅 compose） |
| `CC_MAX_BODY_MB` | `100` | 请求体大小上限（MB），超限请求返回 `HTTP 413` |
| `CC_CLIENT_STALL_MS` | 空（禁用）| 下游写阻塞超过该毫秒数则断开该客户端并中止上游请求，见[僵死连接](#僵死连接既不读也不断开) |
| `CC_STREAM_IDLE_MS` | `30000` | 流式上游读空闲超时（毫秒），见[上游空闲超时](#上游空闲超时) |
| `CC_NONSTREAM_IDLE_MS` | `90000` | 非流式上游读空闲超时（毫秒）|
| `CC_MAX_INFLIGHT` | `0`（不限）| 进程内在途请求上限，超限返回 `503` + `Retry-After`，见[在途上限](#在途请求上限可选) |

## 在途请求上限（可选）

**默认关闭**（`CC_MAX_INFLIGHT` 未设置 = 不限制并发），既有行为不变。

本项目定位是**纯反代层**，并发控制属于下游 —— 按 IP / 按 key 的限流请用反向代理（见[内存与部署](#内存与部署)里的 `limit_conn`）。
本项**不是**那套方案的替代品，只为「不挂反代裸跑」提供一个**进程内、仅全局**的兜底：

```bash
CC_MAX_INFLIGHT=32 ./cc-proxy    # 最多同时处理 32 个请求
```

超限时快速返回 `503` + `Retry-After: 5` + `type: server_busy` —— OpenAI / Anthropic 官方 SDK 认得这个组合会自动退避重试，而不是拿到连接被重置。`/health` 与 `/` 不计入、也不受限制，避免探活与编排器因业务繁忙收到 503。

**为什么需要它**：内存 = `在途数 × (0.13MB + 5.5 × body_MB)`。`CC_MAX_BODY_MB` 只管住**单请求**量级，乘数无人管 —— 默认 100MB 时 N 个并发最坏可达 N × 550MB。

> ⚠️ 开启本项**不等于**内存安全：32 × 550MB 仍远超小机器容量。要拿到硬性上界，需**同时**下调 `CC_MAX_BODY_MB`。

## 上游空闲超时

两个上游读空闲看门狗，超时后返回 `429`（带 `retry_after`）让 SDK 自动重试：

| 环境变量 | 默认 | 作用于 |
|---|---|---|
| `CC_STREAM_IDLE_MS` | `30000` | 流式请求 |
| `CC_NONSTREAM_IDLE_MS` | `90000` | 非流式请求 |

**语义**：只计「`reader.read()` 的等待时间」，每收到一个 chunk 就重置 —— **不是整个请求的总时长**。
只要上游在持续吐流就不会触发，哪怕单个请求已经跑了几十分钟。

**默认值与官方 CLI 不一致，这是已知取舍**（[#19](https://github.com/MAXeaglet/commandcode-proxy/issues/19)）：
官方 CLI 对上游**没有任何** idle timeout —— 反编译 `command-code@1.50.0` 可见所有 `createApiClient({ baseUrl })` 调用点都未传 `timeout`，实测 700+ 秒的停顿可正常完成。
本代理保留 30s 是为了兜住真正死掉的连接；代价是**推理模型的长思考停顿可能被误杀**。

若遇到「`429 Response timeout`」「`zero output tokens`」且日志里 `elapsedMs ≈ 30000`、`bytesReceived = 0`，
说明是看门狗误杀了 prefill / 首 token 阶段的正常停顿 —— 调大即可：

```bash
CC_STREAM_IDLE_MS=300000 ./cc-proxy      # 5 分钟
```

> ⚠️ 误杀的成本不止一次失败：被 abort 后返回 `429 + retry_after`，SDK 会自动重试，
> 而重试等于**完整重发整个上下文**，长会话下每次误杀都要重付一次全量 prefill。

## 内存与部署

Go 实现的请求体最多保留约 2 份副本（原始字节 + 解析结构），远低于 Node 字符串/Buffer 管线的 5~7 份；四种协议翻译全程流式，不整段缓冲响应。流式背压向上游传导：客户端停止读取 → SSE 写出停止 → 上游读取停顿 → 空闲看门狗最终断连，响应不会在内存里无界堆积。

部署规则不变：

### 请求体上限

超过 `CC_MAX_BODY_MB`（默认 **100MB**）的请求在解析前即拒绝并返回 `HTTP 413`。该上限是**按请求**的，公网部署还需在反向代理层限制并发。

### nginx 反代建议

```nginx
map $http_authorization $cc_key { default $http_authorization; "" $http_x_api_key; }
map "" $cc_global_key { default "global"; }

limit_conn_zone $binary_remote_addr zone=cc_ip:10m;
limit_conn_zone $cc_key             zone=cc_key:10m;
limit_conn_zone $cc_global_key      zone=cc_global:10m;

location /v1/ {
    client_max_body_size 4m;   # 需 <= CC_MAX_BODY_MB
    limit_conn cc_ip     8;
    limit_conn cc_key    4;
    limit_conn cc_global 32;   # 这一项就是内存天花板
    limit_conn_status 429;
    proxy_pass http://127.0.0.1:3050;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_read_timeout 300s;   # 需大于 30s 的流空闲超时
}
```

### 僵死连接（既不读也不断开）

僵死客户端会持有上游连接。启用写死线后自动丢弃（只要客户端在推进 drain 就不会触发）：

```bash
CC_CLIENT_STALL_MS=60000 ./cc-proxy
```

更稳妥的封顶仍在反向代理层（`limit_conn`），因为只有它知道该部署能承受多少并发。

### 其它注意事项

- **日志文件**：建议 `logFile` 留空、从 stdout 收集。
- **systemd 兜底**：配 `MemoryMax=`，让超限杀掉 proxy 而不是 `sshd`/`nginx`。
- **多账号 + 多实例**：per-key 的 session 与指纹状态在进程内存中。同一个 API key 打到两个实例会得到两个不同 session 与**两个不同设备指纹**，上游会看到「一个账号在多台机器上」。横向扩展请按 API key 做一致性哈希（`hash $cc_key consistent`），不要轮询。

## 免责声明

本项目仅供**学习和研究**使用。

- **非官方**：本项目与 Command Code 无任何关联，非官方产品。
- **个人使用**：使用者应自行承担所有责任。请遵守 [Command Code 服务条款](https://commandcode.ai/tos)。
- **API Key**：本项目不会收集、上传或泄露你的 API Key。Key 通过每次请求的 `Authorization: Bearer <key>` 或 `x-api-key` 头传入，日志中不记录；`config.json` 中的可选 `apiKey` 字段仅作本地兜底，不会离开你的机器。
- **合规性**：协议基于对本地 CLI 网络流量的被动观察，未对服务端进行任何未授权访问、破解或篡改。
- **账号风险**：建议和正常 CLI 使用频率保持一致，超高并发调用可能触发风控。

---

[Linux.do](https://linux.do)

## 开发

```bash
go vet ./...
go test -race ./...
go build -o cc-proxy .
python testdata/stub_cc.py 9701 &     # CC 上游桩
CC_API_BASE=http://127.0.0.1:9701 CC_UPSTREAM_KEYS=user_test ./cc-proxy
```

[Linux.do](https://linux.do)
