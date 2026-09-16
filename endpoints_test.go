package main

import (
	"encoding/json"
	"testing"
)

// convertResponsesToChat 必须接受标准 Responses 数组项：不带 type 字段的
// {role, content} 是协议规范形态，静默丢弃会让上游只收到 system 消息。
func TestConvertResponsesToChatAcceptsUntypedMessageItems(t *testing.T) {
	req := &responsesRequest{
		Model: "m1",
		Input: json.RawMessage(`[
			{"role":"user","content":"say alpha"},
			{"role":"assistant","content":"earlier reply"},
			{"role":"user","content":[{"type":"input_text","text":"say beta"}]}
		]`),
	}
	got := convertResponsesToChat(req)
	if len(got.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %s", len(got.Messages), mustJSON(got.Messages))
	}
	if got.Messages[0].Role != "user" || responsesPlain(got.Messages[0].Content) != "say alpha" {
		t.Errorf("msg0 = %s/%s, want user/say alpha", got.Messages[0].Role, responsesPlain(got.Messages[0].Content))
	}
	if got.Messages[1].Role != "assistant" || responsesPlain(got.Messages[1].Content) != "earlier reply" {
		t.Errorf("msg1 = %s/%s, want assistant/earlier reply", got.Messages[1].Role, responsesPlain(got.Messages[1].Content))
	}
	if got.Messages[2].Role != "user" || responsesPlain(got.Messages[2].Content) != "say beta" {
		t.Errorf("msg2 = %s/%s, want user/say beta", got.Messages[2].Role, responsesPlain(got.Messages[2].Content))
	}
}

// 带 type 的 item 行为保持不变（typed message / reasoning / function_call / function_call_output）。
func TestConvertResponsesToChatTypedItemsStillWork(t *testing.T) {
	req := &responsesRequest{
		Model: "m1",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"weather?"},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]`),
		Tools: []responsesTool{{Type: "function", Name: "get_weather"}},
	}
	got := convertResponsesToChat(req)
	var toolMsg *chatMessage
	var assistantToolCalls int
	for i := range got.Messages {
		m := &got.Messages[i]
		switch m.Role {
		case "assistant":
			assistantToolCalls = len(m.ToolCalls)
		case "tool":
			toolMsg = m
		}
	}
	if assistantToolCalls != 1 {
		t.Errorf("assistant tool_calls = %d, want 1", assistantToolCalls)
	}
	if toolMsg == nil || toolMsg.ToolCallID != "call_1" || responsesPlain(toolMsg.Content) != "sunny" {
		t.Errorf("tool result message wrong: %+v", toolMsg)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("tools not converted: %+v", got.Tools)
	}
}

// anthropic image 内容块必须透传（base64 → data URI），不能静默丢弃。
func TestConvertAnthropicToChatKeepsImages(t *testing.T) {
	req := &anthropicRequest{
		Model: "m1",
		Messages: []anthropicMessage{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"text","text":"describe"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
			]`),
		}},
	}
	got := convertAnthropicToChat(req)
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("expected single user message, got %+v", got.Messages)
	}
	var parts []chatContentPart
	if err := json.Unmarshal(got.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content should be parts array: %v", err)
	}
	if len(parts) != 2 || parts[0].Text != "describe" {
		t.Fatalf("text part missing: %+v", parts)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil ||
		parts[1].ImageURL.URL != "data:image/jpeg;base64,QUJD" {
		t.Errorf("image part wrong: %+v", parts[1])
	}
}

func TestAnthropicImageURLVariants(t *testing.T) {
	if got := anthropicImageURL(nil); got != "" {
		t.Errorf("nil source = %q, want empty", got)
	}
	if got := anthropicImageURL(&anthropicImageSource{Type: "url", URL: "https://x/y.png"}); got != "https://x/y.png" {
		t.Errorf("url source = %q", got)
	}
	if got := anthropicImageURL(&anthropicImageSource{Type: "base64", Data: "QQ=="}); got != "data:image/png;base64,QQ==" {
		t.Errorf("base64 default media type = %q", got)
	}
	if got := anthropicImageURL(&anthropicImageSource{Type: "unknown"}); got != "" {
		t.Errorf("unknown source = %q, want empty", got)
	}
}

// responsesPlain 读取 mustJSON 存的字符串内容，用于断言。
func responsesPlain(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
