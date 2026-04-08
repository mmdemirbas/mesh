package gateway

import (
	"encoding/json"
	"testing"
)

func TestTranslateAnthropicRequest_SimpleText(t *testing.T) {
	cfg := &GatewayCfg{ModelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"}}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if out.Model != "gpt-4o" {
		t.Errorf("model = %q, want %q", out.Model, "gpt-4o")
	}
	if *out.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", *out.MaxTokens)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(out.Messages))
	}
	if out.Messages[0].Role != "user" {
		t.Errorf("role = %q, want %q", out.Messages[0].Role, "user")
	}
}

func TestTranslateAnthropicRequest_SystemString(t *testing.T) {
	cfg := &GatewayCfg{}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		System:    json.RawMessage(`"You are helpful."`),
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", out.Messages[0].Role)
	}
	var sysText string
	json.Unmarshal(out.Messages[0].Content, &sysText)
	if sysText != "You are helpful." {
		t.Errorf("system text = %q", sysText)
	}
}

func TestTranslateAnthropicRequest_SystemArray(t *testing.T) {
	cfg := &GatewayCfg{}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		System:    json.RawMessage(`[{"type":"text","text":"Part 1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Part 2"}]`),
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sysText string
	json.Unmarshal(out.Messages[0].Content, &sysText)
	if sysText != "Part 1\n\nPart 2" {
		t.Errorf("system text = %q, want 'Part 1\\n\\nPart 2'", sysText)
	}
}

func TestTranslateAnthropicRequest_DefaultMaxTokens(t *testing.T) {
	cfg := &GatewayCfg{DefaultMaxTokens: 16384}
	req := &MessagesRequest{
		Model: "claude-sonnet-4-6",
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if *out.MaxTokens != 16384 {
		t.Errorf("max_tokens = %d, want 16384", *out.MaxTokens)
	}
}

func TestTranslateAnthropicRequest_ImageBase64(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR..."}}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(content)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var parts []ContentPart
	json.Unmarshal(out.Messages[0].Content, &parts)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if parts[0].Type != "image_url" {
		t.Errorf("type = %q, want image_url", parts[0].Type)
	}
	expected := "data:image/png;base64,iVBOR..."
	if parts[0].ImageURL.URL != expected {
		t.Errorf("url = %q, want %q", parts[0].ImageURL.URL, expected)
	}
}

func TestTranslateAnthropicRequest_ToolUse(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"/tmp/x"}}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []AnthropicMsg{
			{Role: "assistant", Content: json.RawMessage(content)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	msg := out.Messages[0]
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("id = %q, want call_1", tc.ID)
	}
	if tc.Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", tc.Function.Name)
	}
	if tc.Type != "function" {
		t.Errorf("type = %q, want function", tc.Type)
	}
}

func TestTranslateAnthropicRequest_ToolResult(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"tool_result","tool_use_id":"call_1","content":"file contents here"}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(content)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	msg := out.Messages[0]
	if msg.Role != "tool" {
		t.Errorf("role = %q, want tool", msg.Role)
	}
	if msg.ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q, want call_1", msg.ToolCallID)
	}
}

func TestTranslateAnthropicRequest_ToolResultError(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"tool_result","tool_use_id":"call_1","content":"not found","is_error":true}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(content)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var text string
	json.Unmarshal(out.Messages[0].Content, &text)
	if text != "[ERROR] not found" {
		t.Errorf("content = %q, want '[ERROR] not found'", text)
	}
}

func TestTranslateAnthropicRequest_Tools(t *testing.T) {
	cfg := &GatewayCfg{}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Tools: []AnthropicTool{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(out.Tools))
	}
	tool := out.Tools[0]
	if tool.Type != "function" {
		t.Errorf("type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", tool.Function.Name)
	}
	if tool.Function.Strict != nil && *tool.Function.Strict {
		t.Error("strict should not be true")
	}
}

func TestTranslateAnthropicRequest_ToolChoiceAll(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"read"}`, `{"type":"function","function":{"name":"read"}}`},
	}

	for _, tt := range tests {
		result, err := translateAnthropicToolChoice(json.RawMessage(tt.input))
		if err != nil {
			t.Errorf("input %s: %v", tt.input, err)
			continue
		}
		got := string(result)
		if got != tt.want {
			t.Errorf("input %s: got %s, want %s", tt.input, got, tt.want)
		}
	}
}

func TestTranslateAnthropicRequest_Stream(t *testing.T) {
	cfg := &GatewayCfg{}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Stream:    true,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Stream {
		t.Error("stream should be true")
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage should be true")
	}
}

func TestTranslateAnthropicRequest_ThinkingDropped(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"thinking","thinking":"I think..."},{"type":"text","text":"Hello"}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages:  []AnthropicMsg{{Role: "assistant", Content: json.RawMessage(content)}},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var text string
	json.Unmarshal(out.Messages[0].Content, &text)
	if text != "Hello" {
		t.Errorf("content = %q, want 'Hello'", text)
	}
	if len(out.Messages[0].ToolCalls) != 0 {
		t.Error("should have no tool calls")
	}
}

func TestTranslateAnthropicRequest_MetadataUserID(t *testing.T) {
	cfg := &GatewayCfg{}
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages:  []AnthropicMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Metadata:  &AnthropicMeta{UserID: "user-123"},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.User != "user-123" {
		t.Errorf("user = %q, want user-123", out.User)
	}
}

// --- Response tests ---

func TestTranslateOpenAIResponse_SimpleText(t *testing.T) {
	resp := &ChatCompletionResponse{
		ID:    "chatcmpl-123",
		Model: "gpt-4o",
		Choices: []OpenAIChoice{
			{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Hello there"`)},
				FinishReason: "stop",
			},
		},
		Usage: &OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	out, err := translateOpenAIResponse(resp, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}

	if out.Type != "message" {
		t.Errorf("type = %q, want message", out.Type)
	}
	if out.Role != "assistant" {
		t.Errorf("role = %q, want assistant", out.Role)
	}
	if out.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", out.Model)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" {
		t.Fatalf("content = %v", out.Content)
	}
	if out.Content[0].Text != "Hello there" {
		t.Errorf("text = %q, want 'Hello there'", out.Content[0].Text)
	}
	if out.Usage.InputTokens != 10 {
		t.Errorf("input_tokens = %d, want 10", out.Usage.InputTokens)
	}
	if out.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens = %d, want 5", out.Usage.OutputTokens)
	}
}

func TestTranslateOpenAIResponse_ToolCalls(t *testing.T) {
	resp := &ChatCompletionResponse{
		ID:    "chatcmpl-456",
		Model: "gpt-4o",
		Choices: []OpenAIChoice{
			{
				Message: OpenAIMsg{
					Role: "assistant",
					ToolCalls: []OpenAIToolCall{
						{
							ID:   "call_abc",
							Type: "function",
							Function: OpenAIFuncCall{
								Name:      "read_file",
								Arguments: `{"path":"/tmp/x"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
		Usage: &OpenAIUsage{PromptTokens: 20, CompletionTokens: 10},
	}

	out, err := translateOpenAIResponse(resp, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}

	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %v", out.Content)
	}
	tc := out.Content[0]
	if tc.ID != "call_abc" {
		t.Errorf("id = %q, want call_abc", tc.ID)
	}
	if tc.Name != "read_file" {
		t.Errorf("name = %q, want read_file", tc.Name)
	}
}

func TestTranslateOpenAIResponse_FinishReasons(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "end_turn"},
		{"unknown", "end_turn"},
	}

	for _, tt := range tests {
		got := mapOpenAIFinishReason(tt.reason)
		if got != tt.want {
			t.Errorf("mapOpenAIFinishReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestTranslateOpenAIResponse_NoUsage(t *testing.T) {
	resp := &ChatCompletionResponse{
		ID:    "chatcmpl-789",
		Model: "gpt-4o",
		Choices: []OpenAIChoice{
			{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Hi"`)},
				FinishReason: "stop",
			},
		},
	}

	out, err := translateOpenAIResponse(resp, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if out.Usage.InputTokens != 0 || out.Usage.OutputTokens != 0 {
		t.Errorf("usage should be zeros, got %+v", out.Usage)
	}
}

func TestTranslateOpenAIResponse_IDPrefix(t *testing.T) {
	resp := &ChatCompletionResponse{
		ID: "chatcmpl-123",
		Choices: []OpenAIChoice{
			{Message: OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Hi"`)}, FinishReason: "stop"},
		},
	}
	out, _ := translateOpenAIResponse(resp, "m")
	if out.ID != "msg_chatcmpl-123" {
		t.Errorf("id = %q, want msg_chatcmpl-123", out.ID)
	}

	resp.ID = "msg_already"
	out, _ = translateOpenAIResponse(resp, "m")
	if out.ID != "msg_already" {
		t.Errorf("id = %q, want msg_already", out.ID)
	}
}

func TestTranslateAnthropicRequest_EmptyToolInput(t *testing.T) {
	cfg := &GatewayCfg{}
	content := `[{"type":"tool_use","id":"call_1","name":"get_time","input":null}]`
	req := &MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []AnthropicMsg{
			{Role: "assistant", Content: json.RawMessage(content)},
		},
	}

	out, err := translateAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	tc := out.Messages[0].ToolCalls[0]
	if tc.Function.Arguments != "{}" {
		t.Errorf("arguments = %q, want '{}'", tc.Function.Arguments)
	}
}
