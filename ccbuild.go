package main

import (
	"bytes"
	"encoding/json"
)

// ── 内部中间表示（OpenAI Chat 格式） ─────────────────
//
// Anthropic /v1/messages 与 OpenAI /v1/responses 都先转换成 chatRequest，
// 再走同一条 CC 请求构建管线 —— 转换逻辑只有一处，三个端点行为天然一致。

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stream              bool            `json:"stream"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	PromptCacheKey      string          `json:"prompt_cache_key"`
	Stop                json.RawMessage `json:"stop"`
	User                string          `json:"user"`
}

func (r *chatRequest) effectiveMaxTokens() int {
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	return 0
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	// 兼容客户端把思考内容放在 thinking 字段（旧实现同样回传）
	Thinking  string         `json:"thinking"`
	ToolCalls []chatToolCall `json:"tool_calls"`
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (t *chatTool) name() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

func (t *chatTool) description() string {
	if t.Function != nil && t.Function.Description != "" {
		return t.Function.Description
	}
	return t.Description
}

func (t *chatTool) parameters() json.RawMessage {
	if t.Function != nil && len(t.Function.Parameters) > 0 {
		return t.Function.Parameters
	}
	if len(t.Parameters) > 0 {
		return t.Parameters
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// ── 内容解析 ────────────────────────────────────────

type chatContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Content  string `json:"content"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	Reasoning    string          `json:"reasoning"`
	CacheControl json.RawMessage `json:"cache_control"`
}

func (p *chatContentPart) textOf() string {
	if p.Text != "" {
		return p.Text
	}
	return p.Content
}

// messageText 把 content（字符串或块数组）压平成纯文本，用于 system 提示。
func messageText(content json.RawMessage) string {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
		return ""
	}
	if trimmed[0] == '[' {
		var parts []chatContentPart
		if err := json.Unmarshal(trimmed, &parts); err == nil {
			var sb bytes.Buffer
			for i := range parts {
				if i > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(parts[i].textOf())
			}
			return sb.String()
		}
	}
	return string(trimmed)
}

func contentParts(content json.RawMessage) []chatContentPart {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return []chatContentPart{{Type: "text", Text: s}}
		}
		return nil
	}
	if trimmed[0] == '[' {
		var parts []chatContentPart
		if err := json.Unmarshal(trimmed, &parts); err == nil {
			return parts
		}
	}
	return []chatContentPart{{Type: "text", Text: string(trimmed)}}
}

// ── CC 请求体 ───────────────────────────────────────

type ccRequestBody struct {
	Config         ccConfig `json:"config"`
	Memory         any      `json:"memory"`
	Taste          any      `json:"taste"`
	Skills         string   `json:"skills"`
	PermissionMode string   `json:"permissionMode"`
	Params         ccParams `json:"params"`
}

type ccConfig struct {
	WorkingDir    string `json:"workingDir"`
	Date          string `json:"date"`
	Environment   string `json:"environment"`
	Structure     []any  `json:"structure"`
	IsGitRepo     bool   `json:"isGitRepo"`
	CurrentBranch string `json:"currentBranch"`
	MainBranch    string `json:"mainBranch"`
	GitStatus     string `json:"gitStatus"`
	RecentCommits []any  `json:"recentCommits"`
}

type ccParams struct {
	Model             string          `json:"model"`
	Messages          []ccMessage     `json:"messages"`
	MaxTokens         int             `json:"max_tokens"`
	Stream            bool            `json:"stream"`
	System            string          `json:"system,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Tools             []ccTool        `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

type ccMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type ccTextPart struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl *ccCacheControl `json:"cache_control,omitempty"`
}

type ccImagePart struct {
	Type  string `json:"type"`
	Image string `json:"image"`
}

type ccReasoningPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ccToolCallPart struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Input      json.RawMessage `json:"input"`
}

type ccToolResultPart struct {
	Type       string             `json:"type"`
	ToolCallID string             `json:"toolCallId"`
	ToolName   string             `json:"toolName"`
	Output     ccToolResultOutput `json:"output"`
}

type ccToolResultOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type ccCacheControl struct {
	Type string `json:"type"`
}

type ccTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

const defaultMaxTokens = 64000
const hardMaxTokens = 200000

// buildCCBody 把中间表示翻译成 CC /alpha/generate 的请求体。
func buildCCBody(cfg *Config, req *chatRequest) ([]byte, error) {
	toolNames := make(map[string]string, 8)
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}

	systemParts := make([]string, 0, 2)
	out := make([]ccMessage, 0, len(req.Messages))
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			systemParts = append(systemParts, messageText(m.Content))
		case "assistant":
			parts := make([]any, 0, 4)
			// 思考内容必须回传：CC 在 thinking 模式下校验 reasoning 是否随历史带回，
			// 丢弃会让上游直接拒绝。顺序也必须与 CLI 抓包一致：[reasoning, text, tool-call]。
			reasoning := m.ReasoningContent
			if reasoning == "" {
				reasoning = m.Thinking
			}
			if reasoning != "" {
				parts = append(parts, ccReasoningPart{Type: "reasoning", Text: reasoning})
			}
			for _, p := range contentParts(m.Content) {
				switch {
				case p.Type == "reasoning":
					if reasoning == "" {
						t := p.Reasoning
						if t == "" {
							t = p.textOf()
						}
						parts = append(parts, ccReasoningPart{Type: "reasoning", Text: t})
					}
				case p.Type == "text" || p.textOf() != "":
					if text := p.textOf(); text != "" {
						parts = append(parts, ccTextPart{Type: "text", Text: text})
					}
				}
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, ccToolCallPart{
					Type:       "tool-call",
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					Input:      toolArgsJSON(rawOrEmpty(json.RawMessage(tc.Function.Arguments))),
				})
			}
			out = append(out, ccMessage{Role: "assistant", Content: parts})
		case "tool", "function":
			name := toolNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			out = append(out, ccMessage{Role: "tool", Content: []any{ccToolResultPart{
				Type:       "tool-result",
				ToolCallID: m.ToolCallID,
				ToolName:   name,
				Output:     ccToolResultOutput{Type: "text", Value: toolResultText(m.Content)},
			}}})
		default: // user 及未知 role 兜底
			out = append(out, ccMessage{Role: "user", Content: userParts(m.Content)})
		}
	}

	// prompt_cache_key → 在第一条 user 消息的最后一个 text 块上打 cache_control 标记，
	// 让上游按这个边界复用 prompt 缓存（issue #9 的显式亲和性）。
	if req.PromptCacheKey != "" && !hasCacheMarker(out) {
		for i := range out {
			if out[i].Role != "user" {
				continue
			}
			for j := len(out[i].Content) - 1; j >= 0; j-- {
				if tp, ok := out[i].Content[j].(ccTextPart); ok {
					tp.CacheControl = &ccCacheControl{Type: "ephemeral"}
					out[i].Content[j] = tp
					break
				}
			}
			break
		}
	}

	maxTok := req.effectiveMaxTokens()
	if maxTok <= 0 {
		maxTok = defaultMaxTokens
	}
	if maxTok > hardMaxTokens {
		maxTok = hardMaxTokens
	}

	model := req.Model
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}

	body := ccRequestBody{
		Config: ccConfig{
			WorkingDir:    processWorkingDir,
			Date:          todayString(),
			Environment:   environmentString(),
			Structure:     []any{},
			RecentCommits: []any{},
		},
		Memory:         nil,
		Taste:          nil,
		Skills:         "",
		PermissionMode: "standard",
		Params: ccParams{
			Model:     model,
			Messages:  out,
			MaxTokens: maxTok,
			Stream:    true, // CC API 始终返回流
		},
	}

	systemPrompt := joinNonEmpty(systemParts, "\n")
	if systemPrompt != "" {
		body.Params.System = systemPrompt
	} else if cfg.EmptySystemPlaceholder {
		// 上游在 params.system 缺省时会注入自身约 7.5K token 的默认提示词，
		// 既产生大量 cached tokens 又污染对话（issue #17）。发一个空格占位即可绕过。
		body.Params.System = " "
	}
	if req.Temperature != nil {
		body.Params.Temperature = req.Temperature
	}
	if req.ReasoningEffort != "" {
		body.Params.ReasoningEffort = req.ReasoningEffort
	}
	if len(req.Tools) > 0 {
		tools := make([]ccTool, 0, len(req.Tools))
		for i := range req.Tools {
			t := &req.Tools[i]
			typ := t.Type
			if typ == "" {
				typ = "function"
			}
			tools = append(tools, ccTool{
				Type:        typ,
				Name:        t.name(),
				Description: t.description(),
				InputSchema: t.parameters(),
			})
		}
		body.Params.Tools = tools
	}
	if tc := convertToolChoice(req.ToolChoice); len(tc) > 0 {
		body.Params.ToolChoice = tc
	}
	if req.ParallelToolCalls != nil {
		body.Params.ParallelToolCalls = req.ParallelToolCalls
	}

	return json.Marshal(body)
}

func hasCacheMarker(msgs []ccMessage) bool {
	for i := range msgs {
		for j := range msgs[i].Content {
			if tp, ok := msgs[i].Content[j].(ccTextPart); ok && tp.CacheControl != nil {
				return true
			}
		}
	}
	return false
}

func userParts(content json.RawMessage) []any {
	parts := contentParts(content)
	out := make([]any, 0, len(parts))
	for i := range parts {
		p := &parts[i]
		if p.Type == "image_url" && p.ImageURL != nil {
			out = append(out, ccImagePart{Type: "image", Image: p.ImageURL.URL})
			continue
		}
		if p.Type == "image" && p.Text == "" {
			out = append(out, ccImagePart{Type: "image", Image: p.Content})
			continue
		}
		out = append(out, ccTextPart{Type: "text", Text: p.textOf()})
	}
	if len(out) == 0 {
		out = append(out, ccTextPart{Type: "text", Text: ""})
	}
	return out
}

func toolResultText(content json.RawMessage) string {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
		return string(trimmed)
	case '[':
		var parts []chatContentPart
		if err := json.Unmarshal(trimmed, &parts); err == nil {
			var sb bytes.Buffer
			for i := range parts {
				sb.WriteString(parts[i].textOf())
			}
			return sb.String()
		}
	}
	return string(trimmed)
}

// convertToolChoice 把 OpenAI 的 tool_choice 翻成 CC（Anthropic 风格）。
func convertToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil
		}
		switch s {
		case "auto", "none":
			return json.RawMessage(`{"type":"` + s + `"}`)
		case "required", "any":
			return json.RawMessage(`{"type":"any"}`)
		default:
			return json.RawMessage(`{"type":"auto"}`)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" {
		name := obj.Name
		if obj.Function != nil && obj.Function.Name != "" {
			name = obj.Function.Name
		}
		b, _ := json.Marshal(map[string]string{"type": "tool", "name": name})
		return b
	}
	// 已经是 CC/Anthropic 形状：原样透传
	return trimmed
}

func joinNonEmpty(parts []string, sep string) string {
	var out []byte
	for _, p := range parts {
		if p == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, sep...)
		}
		out = append(out, p...)
	}
	return string(out)
}
