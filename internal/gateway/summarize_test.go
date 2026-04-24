package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEstimateTokens_Basic(t *testing.T) {
	t.Parallel()
	req := &MessagesRequest{
		System: json.RawMessage(`"You are a helpful assistant."`),
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Hello, how are you?"`)},
			{Role: "assistant", Content: json.RawMessage(`"I'm doing well!"`)},
		},
	}
	tokens := estimateTokens(req)
	// Rough check: total bytes / 3.5. Not exact, just verify it's reasonable.
	if tokens < 10 || tokens > 100 {
		t.Errorf("estimateTokens = %d, expected roughly 10-100", tokens)
	}
}

func TestEstimateTokens_LargeRequest(t *testing.T) {
	t.Parallel()
	// Simulate ~700KB of content → should estimate ~200K tokens
	largeContent := strings.Repeat("a", 700_000)
	req := &MessagesRequest{
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"` + largeContent + `"`)},
		},
	}
	tokens := estimateTokens(req)
	if tokens < 150_000 || tokens > 250_000 {
		t.Errorf("estimateTokens = %d, expected 150K-250K for 700KB input", tokens)
	}
}

func TestSerializeMessages(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"Hello"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"Hi there!"},{"type":"tool_use","name":"read_file","id":"t1","input":{"path":"/foo"}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"file contents here"}]`)},
	}
	out := serializeMessages(msgs)
	if !strings.Contains(out, "USER:") {
		t.Error("missing USER: prefix")
	}
	if !strings.Contains(out, "ASSISTANT:") {
		t.Error("missing ASSISTANT: prefix")
	}
	if !strings.Contains(out, "Hi there!") {
		t.Error("missing text content")
	}
	if !strings.Contains(out, "[tool: read_file") {
		t.Error("missing tool_use serialization")
	}
	if !strings.Contains(out, "[result:") {
		t.Error("missing tool_result serialization")
	}
}

func TestSummarizeMessages_BelowKeepRecent(t *testing.T) {
	t.Parallel()
	req := &MessagesRequest{
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"msg1"`)},
			{Role: "assistant", Content: json.RawMessage(`"msg2"`)},
		},
	}
	result, err := summarizeMessages(context.Background(), req, &ResolvedUpstream{}, 6, silentLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != len(req.Messages) {
		t.Errorf("messages = %d, want %d (unchanged)", len(result), len(req.Messages))
	}
}

func TestSummarizeMessages_WithMockSummarizer(t *testing.T) {
	t.Parallel()

	// Mock summarizer that returns a fixed summary.
	sumServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("summarizer received invalid request: %v", err)
		}
		// Verify the request has system + user messages
		if len(req.Messages) != 2 {
			t.Errorf("summarizer got %d messages, want 2", len(req.Messages))
		}

		resp := ChatCompletionResponse{
			ID: "sum-1", Model: "gemini-2.0-flash",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Summary: User discussed file operations and debugging."`)},
				FinishReason: "stop",
			}},
			Usage: &OpenAIUsage{PromptTokens: 100, CompletionTokens: 20},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer sumServer.Close()

	summarizer := &ResolvedUpstream{
		Cfg:    UpstreamCfg{Name: "sum", Target: sumServer.URL, API: APIOpenAI},
		Client: http.DefaultClient,
	}

	// Build a request with 10 messages (more than keepRecent=4).
	msgs := make([]AnthropicMsg, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = AnthropicMsg{
			Role:    role,
			Content: json.RawMessage(`"message ` + string(rune('A'+i)) + `"`),
		}
	}
	req := &MessagesRequest{Messages: msgs}

	result, err := summarizeMessages(context.Background(), req, summarizer, 4, silentLogger())
	if err != nil {
		t.Fatalf("summarizeMessages: %v", err)
	}

	// Should have: summary (user) + ack (assistant) + 4 recent messages = 6
	// The ack is inserted because recent[0] is user (msg[6] which is index 6, even, so user).
	if len(result) < 5 {
		t.Errorf("result messages = %d, expected at least 5", len(result))
	}

	// First message should be the summary.
	var firstContent string
	_ = json.Unmarshal(result[0].Content, &firstContent)
	if !strings.Contains(firstContent, "Conversation summary") {
		t.Errorf("first message should be summary, got: %s", firstContent[:minInt(len(firstContent), 100)])
	}
	if !strings.Contains(firstContent, "file operations") {
		t.Errorf("summary content not found in first message: %s", firstContent[:minInt(len(firstContent), 100)])
	}
}

func TestCheckAndSummarize_NoLimit(t *testing.T) {
	t.Parallel()
	upstream := &ResolvedUpstream{
		Cfg: UpstreamCfg{ContextWindow: 0}, // no limit
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, result, _, _ := checkAndSummarize(context.Background(), body, upstream, nil, silentLogger())
	if result != contextOK {
		t.Errorf("result = %d, want contextOK", result)
	}
	if string(out) != string(body) {
		t.Error("body should be unchanged")
	}
}

func TestCheckAndSummarize_WithinLimit(t *testing.T) {
	t.Parallel()
	upstream := &ResolvedUpstream{
		Cfg: UpstreamCfg{ContextWindow: 1_000_000}, // very large
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, result, _, _ := checkAndSummarize(context.Background(), body, upstream, nil, silentLogger())
	if result != contextOK {
		t.Errorf("result = %d, want contextOK", result)
	}
	if string(out) != string(body) {
		t.Error("body should be unchanged")
	}
}

func TestCheckAndSummarize_ExceededNoSummarizer(t *testing.T) {
	t.Parallel()
	upstream := &ResolvedUpstream{
		Cfg: UpstreamCfg{ContextWindow: 10}, // very small
	}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 1000) + `"}]}`)
	_, result, info, err := checkAndSummarize(context.Background(), body, upstream, nil, silentLogger())
	if result != contextExceeded {
		t.Errorf("result = %d, want contextExceeded", result)
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if info.OriginalTokens < 100 {
		t.Errorf("estimated = %d, expected > 100", info.OriginalTokens)
	}
	if !strings.Contains(err.Error(), "default_max_tokens only limits output tokens") {
		t.Errorf("error = %v, want context window message", err)
	}
}

func TestCheckAndSummarize_Summarizes(t *testing.T) {
	t.Parallel()

	sumServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatCompletionResponse{
			ID:    "sum-2",
			Model: "gemini-2.0-flash",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Short summary."`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer sumServer.Close()

	upstream := &ResolvedUpstream{Cfg: UpstreamCfg{ContextWindow: 200, Summarizer: "sum"}}
	router := &Router{upstreams: map[string]*ResolvedUpstream{
		"sum": {
			Cfg:    UpstreamCfg{Name: "sum", Target: sumServer.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "gemini-2.0-flash"}},
			Client: http.DefaultClient,
		},
	}}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 700) + `"},{"role":"assistant","content":"ok"},{"role":"user","content":"recent"},{"role":"assistant","content":"still recent"},{"role":"user","content":"more recent"},{"role":"assistant","content":"latest"},{"role":"user","content":"tail"}]}`)

	out, result, info, err := checkAndSummarize(context.Background(), body, upstream, router, silentLogger())
	if err != nil {
		t.Fatalf("checkAndSummarize: %v", err)
	}
	if result != contextSummarized {
		t.Fatalf("result = %d, want contextSummarized", result)
	}
	if info.OriginalTokens <= upstream.Cfg.ContextWindow {
		t.Fatalf("estimated = %d, want > %d", info.OriginalTokens, upstream.Cfg.ContextWindow)
	}
	if !info.Summarized {
		t.Fatal("expected summarized=true")
	}
	if info.EffectiveTokens > upstream.Cfg.ContextWindow {
		t.Fatalf("effective tokens = %d, want <= %d", info.EffectiveTokens, upstream.Cfg.ContextWindow)
	}

	var req MessagesRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal summarized body: %v", err)
	}
	if len(req.Messages) < defaultKeepRecent {
		t.Fatalf("messages after summarization = %d, want >= %d", len(req.Messages), defaultKeepRecent)
	}
	var first string
	_ = json.Unmarshal(req.Messages[0].Content, &first)
	if !strings.Contains(first, "Conversation summary") {
		t.Fatalf("first summarized message = %q, want conversation summary", first)
	}
}

func TestCheckAndSummarize_StillExceededAfterSummarize(t *testing.T) {
	t.Parallel()

	sumServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatCompletionResponse{
			ID:    "sum-3",
			Model: "gemini-2.0-flash",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"` + strings.Repeat("x", 600) + `"`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer sumServer.Close()

	upstream := &ResolvedUpstream{Cfg: UpstreamCfg{ContextWindow: 50, Summarizer: "sum"}}
	router := &Router{upstreams: map[string]*ResolvedUpstream{
		"sum": {
			Cfg:    UpstreamCfg{Name: "sum", Target: sumServer.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "gemini-2.0-flash"}},
			Client: http.DefaultClient,
		},
	}}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 300) + `"},{"role":"assistant","content":"reply"},{"role":"user","content":"tail1"},{"role":"assistant","content":"tail2"},{"role":"user","content":"tail3"},{"role":"assistant","content":"tail4"},{"role":"user","content":"tail5"}]}`)

	_, result, info, err := checkAndSummarize(context.Background(), body, upstream, router, silentLogger())
	if result != contextExceeded {
		t.Fatalf("result = %d, want contextExceeded", result)
	}
	if !info.Summarized {
		t.Fatal("expected summarized=true for still-too-large summarized request")
	}
	if err == nil || !strings.Contains(err.Error(), "still too large after local summarization") {
		t.Fatalf("err = %v, want still-estimated message", err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
