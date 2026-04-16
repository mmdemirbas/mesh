package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranslateOpenAIRequest_GlobModelMap(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{ModelMap: map[string]string{
		"gpt-4o": "claude-opus-4-6",   // exact
		"gpt-4*": "claude-sonnet-4-6", // glob
		"*":      "claude-haiku-4-5",  // catch-all
	}}
	tests := []struct {
		input, want string
	}{
		{"gpt-4o", "claude-opus-4-6"},        // exact
		{"gpt-4o-mini", "claude-sonnet-4-6"}, // glob
		{"gpt-4-turbo", "claude-sonnet-4-6"}, // glob
		{"gemini-pro", "claude-haiku-4-5"},   // catch-all
	}
	for _, tt := range tests {
		maxTok := 100
		req := &ChatCompletionRequest{
			Model:     tt.input,
			MaxTokens: &maxTok,
			Messages:  []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		}
		out, err := translateOpenAIRequest(req, cfg)
		if err != nil {
			t.Fatalf("model %q: %v", tt.input, err)
		}
		if out.Model != tt.want {
			t.Errorf("model %q → %q, want %q", tt.input, out.Model, tt.want)
		}
	}
}

func TestTranslateOpenAIRequest_SimpleText(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{ModelMap: map[string]string{"gpt-4o": "claude-sonnet-4-6"}}
	maxTok := 1024
	req := &ChatCompletionRequest{
		Model:     "gpt-4o",
		MaxTokens: &maxTok,
		Messages: []OpenAIMsg{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", out.Model)
	}
	if out.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", out.MaxTokens)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
}

func TestTranslateOpenAIRequest_SystemExtraction(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMsg{
			{Role: "system", Content: json.RawMessage(`"You are helpful."`)},
			{Role: "system", Content: json.RawMessage(`"Be concise."`)},
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var sysText string
	json.Unmarshal(out.System, &sysText)
	if sysText != "You are helpful.\n\nBe concise." {
		t.Errorf("system = %q", sysText)
	}
	if len(out.Messages) != 1 {
		t.Errorf("messages = %d, want 1 (system extracted)", len(out.Messages))
	}
}

func TestTranslateOpenAIRequest_DefaultMaxTokens(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{DefaultMaxTokens: 16384}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != 16384 {
		t.Errorf("max_tokens = %d, want 16384", out.MaxTokens)
	}
}

func TestTranslateOpenAIRequest_TemperatureClamping(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	temp := 1.5
	req := &ChatCompletionRequest{
		Model:       "gpt-4o",
		Temperature: &temp,
		Messages:    []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if *out.Temperature != 1.0 {
		t.Errorf("temperature = %f, want 1.0", *out.Temperature)
	}
}

func TestTranslateOpenAIRequest_TemperaturePassthrough(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	temp := 0.7
	req := &ChatCompletionRequest{
		Model:       "gpt-4o",
		Temperature: &temp,
		Messages:    []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if *out.Temperature != 0.7 {
		t.Errorf("temperature = %f, want 0.7", *out.Temperature)
	}
}

func TestTranslateOpenAIRequest_ConsecutiveSameRole(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMsg{
			{Role: "user", Content: json.RawMessage(`"First"`)},
			{Role: "user", Content: json.RawMessage(`"Second"`)},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (merged)", len(out.Messages))
	}
	// The merged message should contain both text blocks.
	var blocks []ContentBlock
	json.Unmarshal(out.Messages[0].Content, &blocks)
	if len(blocks) != 2 {
		t.Errorf("blocks = %d, want 2", len(blocks))
	}
}

func TestTranslateOpenAIRequest_ToolMessages(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMsg{
			{Role: "user", Content: json.RawMessage(`"use the tool"`)},
			{Role: "assistant", Content: nil, ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFuncCall{Name: "read", Arguments: `{"path":"x"}`}},
			}},
			{Role: "tool", Content: json.RawMessage(`"file contents"`), ToolCallID: "call_1"},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(out.Messages))
	}
	// Third message should be user with tool_result.
	if out.Messages[2].Role != "user" {
		t.Errorf("role = %q, want user", out.Messages[2].Role)
	}
	var blocks []ContentBlock
	json.Unmarshal(out.Messages[2].Content, &blocks)
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Errorf("blocks = %v", blocks)
	}
	if blocks[0].ToolUseID != "call_1" {
		t.Errorf("tool_use_id = %q, want call_1", blocks[0].ToolUseID)
	}
}

func TestTranslateOpenAIRequest_ImageURL(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	content := `[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc123"}}]`
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(content)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var blocks []ContentBlock
	json.Unmarshal(out.Messages[0].Content, &blocks)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[1].Type != "image" {
		t.Errorf("type = %q, want image", blocks[1].Type)
	}
	if blocks[1].Source.MediaType != "image/png" {
		t.Errorf("media_type = %q, want image/png", blocks[1].Source.MediaType)
	}
	if blocks[1].Source.Data != "abc123" {
		t.Errorf("data = %q, want abc123", blocks[1].Source.Data)
	}
}

func TestTranslateOpenAIRequest_ToolChoiceAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{`"auto"`, `{"type":"auto"}`},
		{`"none"`, `{"type":"none"}`},
		{`"required"`, `{"type":"any"}`},
		{`{"type":"function","function":{"name":"read"}}`, `{"type":"tool","name":"read"}`},
	}

	for _, tt := range tests {
		result, err := translateOpenAIToolChoice(json.RawMessage(tt.input))
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

func TestTranslateOpenAIRequest_StopString(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Stop:     json.RawMessage(`"END"`),
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.StopSequences) != 1 || out.StopSequences[0] != "END" {
		t.Errorf("stop = %v, want [END]", out.StopSequences)
	}
}

func TestTranslateOpenAIRequest_StopArray(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Stop:     json.RawMessage(`["END","STOP"]`),
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.StopSequences) != 2 {
		t.Errorf("stop = %v, want [END, STOP]", out.StopSequences)
	}
}

func TestTranslateOpenAIRequest_Tools(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Tools: []OpenAITool{
			{
				Type: "function",
				Function: OpenAIFunction{
					Name:        "read_file",
					Description: "Read a file",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
				},
			},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(out.Tools))
	}
	if out.Tools[0].Name != "read_file" {
		t.Errorf("name = %q, want read_file", out.Tools[0].Name)
	}
}

func TestTranslateOpenAIRequest_User(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		User:     "user-123",
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Metadata == nil || out.Metadata.UserID != "user-123" {
		t.Errorf("metadata.user_id = %v", out.Metadata)
	}
}

// --- Response tests ---

func TestTranslateAnthropicResponse_SimpleText(t *testing.T) {
	t.Parallel()
	resp := &MessagesResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-6",
		StopReason: "end_turn",
		Content: []ContentBlock{
			{Type: "text", Text: "Hello there"},
		},
		Usage: AnthropicUsage{InputTokens: 10, OutputTokens: 5},
	}

	out, err := translateAnthropicResponse(resp, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}

	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", out.Model)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
	var text string
	json.Unmarshal(out.Choices[0].Message.Content, &text)
	if text != "Hello there" {
		t.Errorf("content = %q, want 'Hello there'", text)
	}
	if out.Usage.PromptTokens != 10 {
		t.Errorf("prompt_tokens = %d, want 10", out.Usage.PromptTokens)
	}
	if out.Usage.CompletionTokens != 5 {
		t.Errorf("completion_tokens = %d, want 5", out.Usage.CompletionTokens)
	}
	if out.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", out.Usage.TotalTokens)
	}
}

func TestTranslateAnthropicResponse_ToolUse(t *testing.T) {
	t.Parallel()
	resp := &MessagesResponse{
		ID:         "msg_456",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []ContentBlock{
			{Type: "tool_use", ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"x"}`)},
		},
		Usage: AnthropicUsage{InputTokens: 20, OutputTokens: 10},
	}

	out, err := translateAnthropicResponse(resp, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}

	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", out.Choices[0].FinishReason)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(out.Choices[0].Message.ToolCalls))
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("id = %q, want call_1", tc.ID)
	}
	if tc.Function.Name != "read" {
		t.Errorf("name = %q, want read", tc.Function.Name)
	}
}

func TestTranslateAnthropicResponse_StopReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		want   string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"unknown", "stop"},
	}

	for _, tt := range tests {
		got := mapAnthropicStopReason(tt.reason)
		if got != tt.want {
			t.Errorf("mapAnthropicStopReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestTranslateAnthropicResponse_IDPrefix(t *testing.T) {
	t.Parallel()
	resp := &MessagesResponse{
		ID:      "msg_123",
		Content: []ContentBlock{{Type: "text", Text: "Hi"}},
	}
	out, _ := translateAnthropicResponse(resp, "m")
	// msg_123 already has prefix "chatcmpl-" will be prepended? No: ensurePrefix checks for "chatcmpl-".
	// "msg_123" doesn't start with "chatcmpl-", so it becomes "chatcmpl-msg_123".
	if out.ID != "chatcmpl-msg_123" {
		t.Errorf("id = %q, want chatcmpl-msg_123", out.ID)
	}
}

func TestTranslateAnthropicResponse_ThinkingDropped(t *testing.T) {
	t.Parallel()
	resp := &MessagesResponse{
		ID:         "msg_789",
		StopReason: "end_turn",
		Content: []ContentBlock{
			{Type: "thinking", Thinking: "I think..."},
			{Type: "text", Text: "Hello"},
		},
	}

	out, err := translateAnthropicResponse(resp, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}

	// Should have text only, thinking dropped.
	var text string
	json.Unmarshal(out.Choices[0].Message.Content, &text)
	if text != "Hello" {
		t.Errorf("content = %q, want Hello", text)
	}
	if len(out.Choices[0].Message.ToolCalls) != 0 {
		t.Error("should have no tool calls")
	}
}

func TestTranslateOpenAIRequest_DeveloperRole(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMsg{
			{Role: "developer", Content: json.RawMessage(`"Be helpful"`)},
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var sysText string
	json.Unmarshal(out.System, &sysText)
	if sysText != "Be helpful" {
		t.Errorf("system = %q, want 'Be helpful'", sysText)
	}
}

func TestTranslateOpenAIRequest_ImageURL_PlainURL(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	content := `[{"type":"image_url","image_url":{"url":"https://example.com/photo.jpg"}}]`
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(content)}},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var blocks []ContentBlock
	json.Unmarshal(out.Messages[0].Content, &blocks)
	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("blocks = %v", blocks)
	}
	if blocks[0].Source.Type != "url" {
		t.Errorf("source.type = %q, want url", blocks[0].Source.Type)
	}
	if blocks[0].Source.URL != "https://example.com/photo.jpg" {
		t.Errorf("source.url = %q", blocks[0].Source.URL)
	}
}

func TestTranslateOpenAIRequest_ConsecutiveAssistantMerge(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMsg{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"First"`)},
			{Role: "assistant", Content: json.RawMessage(`"Second"`)},
		},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// user + merged assistant = 2 messages.
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (assistant merged)", len(out.Messages))
	}
	var blocks []ContentBlock
	json.Unmarshal(out.Messages[1].Content, &blocks)
	if len(blocks) != 2 {
		t.Errorf("merged assistant blocks = %d, want 2", len(blocks))
	}
}

func TestTranslateOpenAIRequest_EmptyMessages(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMsg{},
	}

	out, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 0 {
		t.Errorf("messages = %d, want 0", len(out.Messages))
	}
}

func TestTranslateOpenAIRequest_NGreaterThan1Rejected(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	n := 3
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		N:        &n,
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}
	_, err := translateOpenAIRequest(req, cfg)
	if err == nil {
		t.Fatal("expected error for n > 1")
	}
}

func TestTranslateOpenAIRequest_N1Accepted(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}
	n := 1
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		N:        &n,
		Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}
	_, err := translateOpenAIRequest(req, cfg)
	if err != nil {
		t.Fatalf("unexpected error for n=1: %v", err)
	}
}

func TestTranslateOpenAIRequest_ResponseFormat(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{}

	build := func(rf string, withSys bool) *ChatCompletionRequest {
		req := &ChatCompletionRequest{Model: "gpt-4o", Messages: []OpenAIMsg{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
		if withSys {
			req.Messages = append([]OpenAIMsg{{Role: "system", Content: json.RawMessage(`"You are concise."`)}}, req.Messages...)
		}
		if rf != "" {
			req.ResponseFormat = json.RawMessage(rf)
		}
		return req
	}

	tests := []struct {
		name        string
		req         *ChatCompletionRequest
		wantSysSubs []string
		wantNoSys   bool
	}{
		{
			name:        "json_object_no_existing_system",
			req:         build(`{"type":"json_object"}`, false),
			wantSysSubs: []string{"valid JSON only"},
		},
		{
			name:        "json_object_appended_to_existing_system",
			req:         build(`{"type":"json_object"}`, true),
			wantSysSubs: []string{"You are concise.", "valid JSON only"},
		},
		{
			name:        "json_schema_with_schema_embeds_schema",
			req:         build(`{"type":"json_schema","json_schema":{"name":"Result","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}}`, false),
			wantSysSubs: []string{"JSON Schema", `"answer"`},
		},
		{
			name:      "type_text_is_noop",
			req:       build(`{"type":"text"}`, false),
			wantNoSys: true,
		},
		{
			name:      "missing_response_format_is_noop",
			req:       build("", false),
			wantNoSys: true,
		},
		{
			name:      "unknown_type_is_dropped",
			req:       build(`{"type":"protobuf"}`, false),
			wantNoSys: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := translateOpenAIRequest(tt.req, cfg)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if tt.wantNoSys {
				if len(out.System) != 0 {
					var s string
					_ = json.Unmarshal(out.System, &s)
					t.Errorf("expected no system prompt, got %q", s)
				}
				return
			}
			var sys string
			if err := json.Unmarshal(out.System, &sys); err != nil {
				t.Fatalf("system not a string: %v", err)
			}
			for _, want := range tt.wantSysSubs {
				if !strings.Contains(sys, want) {
					t.Errorf("system %q does not contain %q", sys, want)
				}
			}
		})
	}
}
