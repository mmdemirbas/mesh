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
	body, _ := json.Marshal(req)
	tokens := estimateTokens(body)
	// Rough sanity bound: small request should yield a small count.
	if tokens < 10 || tokens > 200 {
		t.Errorf("estimateTokens = %d, expected roughly 10-200", tokens)
	}
}

func TestEstimateTokens_LargeRequest(t *testing.T) {
	t.Parallel()
	// Simulate ~700KB of content. Whole-body estimator at 4.5 B/tok gives
	// ~155K tokens; keep the window wide so it survives divisor retunes
	// without changing the test along with the constant.
	largeContent := strings.Repeat("a", 700_000)
	req := &MessagesRequest{
		Messages: []AnthropicMsg{
			{Role: "user", Content: json.RawMessage(`"` + largeContent + `"`)},
		},
	}
	body, _ := json.Marshal(req)
	tokens := estimateTokens(body)
	if tokens < 100_000 || tokens > 300_000 {
		t.Errorf("estimateTokens = %d, expected 100K-300K for 700KB input", tokens)
	}
}

func TestEstimateTokens_EmptyBody(t *testing.T) {
	t.Parallel()
	if got := estimateTokens(nil); got != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", got)
	}
	if got := estimateTokens([]byte{}); got != 0 {
		t.Errorf("estimateTokens(empty) = %d, want 0", got)
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
	result, err := summarizeMessages(context.Background(), req, &ResolvedUpstream{}, 6, silentLogger(), nil)
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

	result, err := summarizeMessages(context.Background(), req, summarizer, 4, silentLogger(), nil)
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

// TestSafeCutoff_NoToolPairs: with no tool_use/tool_result blocks, the
// safe cutoff is exactly the desired cutoff.
func TestSafeCutoff_NoToolPairs(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`"b"`)},
		{Role: "user", Content: json.RawMessage(`"c"`)},
		{Role: "assistant", Content: json.RawMessage(`"d"`)},
	}
	if got := safeCutoff(msgs, 2); got != 2 {
		t.Errorf("safeCutoff = %d, want 2", got)
	}
}

// TestSafeCutoff_PairInsidePrefix: tool_use and tool_result both before
// the cutoff — safe, cutoff unchanged.
func TestSafeCutoff_PairInsidePrefix(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"go"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu1","name":"Read","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"done"`)},
		{Role: "user", Content: json.RawMessage(`"more"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
	}
	// Desired cutoff = 4 keeps last 2; pair (indices 1-2) fully in prefix.
	if got := safeCutoff(msgs, 4); got != 4 {
		t.Errorf("safeCutoff = %d, want 4 (pair fully in prefix is safe)", got)
	}
}

// TestSafeCutoff_PairInsideRecent: tool_use and tool_result both after
// the cutoff — safe, cutoff unchanged.
func TestSafeCutoff_PairInsideRecent(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`"b"`)},
		{Role: "user", Content: json.RawMessage(`"c"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu9","name":"Read","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu9","content":"ok"}]`)},
	}
	// Desired cutoff = 3 keeps the pair at indices 3-4 together.
	if got := safeCutoff(msgs, 3); got != 3 {
		t.Errorf("safeCutoff = %d, want 3 (pair fully in recent is safe)", got)
	}
}

// TestSafeCutoff_PairSplitAcrossBoundary: tool_use before cutoff,
// tool_result after. Unsafe — cutoff must move back to keep the pair
// together.
func TestSafeCutoff_PairSplitAcrossBoundary(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tuA","name":"Read","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tuA","content":"x"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"last"`)},
	}
	// Desired cutoff = 2 would split: tool_use at idx 1 in prefix,
	// tool_result at idx 2 in recent.
	// Safe cutoff must be 1 (tool_use also moves to recent) or 0.
	got := safeCutoff(msgs, 2)
	if got == 2 {
		t.Fatalf("safeCutoff = 2, expected move-back for tool-safety")
	}
	if got != 1 {
		t.Errorf("safeCutoff = %d, want 1 (tool_use moves to recent)", got)
	}
}

// TestSafeCutoff_ChainedPairs: multiple consecutive tool_use/result
// pairs; cutoff must move back past all of them if any single pair
// would be split.
func TestSafeCutoff_ChainedPairs(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"x"}]`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t2","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t2","content":"y"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"done"`)},
	}
	// Desired cutoff = 4 splits t2's pair (tool_use idx 3 in prefix,
	// tool_result idx 4 in recent). Expected: cutoff 3 (tool_use t2 also
	// moves to recent; pair t1 at indices 1-2 is fully in prefix — safe).
	got := safeCutoff(msgs, 4)
	if got != 3 {
		t.Errorf("safeCutoff = %d, want 3 (t2 pair joins recent, t1 pair stays in prefix)", got)
	}
}

// TestSafeCutoff_ParallelToolUsesSameTurn: a single assistant message
// with multiple tool_use blocks. All tool_results land in the next user
// message. Cutoff must not split any of them.
func TestSafeCutoff_ParallelToolUsesSameTurn(t *testing.T) {
	t.Parallel()
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"p1","name":"R","input":{}},{"type":"tool_use","id":"p2","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"p1","content":"x"},{"type":"tool_result","tool_use_id":"p2","content":"y"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"done"`)},
	}
	// Desired cutoff = 2 splits both p1 and p2. Safe cutoff = 1.
	got := safeCutoff(msgs, 2)
	if got != 1 {
		t.Errorf("safeCutoff = %d, want 1 (both parallel tool_uses must stay with tool_results)", got)
	}
}

// TestSafeCutoff_NoSafeCutExists: a pathological case where every
// prefix-split would break some tool pair. Returns 0.
func TestSafeCutoff_NoSafeCutExists(t *testing.T) {
	t.Parallel()
	// tool_use at msg 0, tool_result at msg 1, tool_use at msg 1 (well,
	// can't really happen — but construct: tool_use in msg 0, tool_result
	// in msg 2, and another tool_use in msg 1 with tool_result in msg 3).
	// Actually the only way to force "no safe cut" is: every prefix length
	// ≥ 1 has some tool_use whose tool_result is in the suffix. Easiest:
	// a tool_use in msg 0 with tool_result in the very last message.
	msgs := []AnthropicMsg{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tX","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`"middle"`)},
		{Role: "assistant", Content: json.RawMessage(`"more"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tX","content":"x"}]`)},
	}
	// For any cutoff ∈ {1,2,3}: tool_use at idx 0 in prefix, tool_result
	// at idx 3 in recent. Unsafe. Only cutoff = 0 is safe.
	if got := safeCutoff(msgs, 3); got != 0 {
		t.Errorf("safeCutoff = %d, want 0 (no safe cut)", got)
	}
}

// TestSummarizeMessages_DoesNotSplitToolPair is the end-to-end regression:
// when the natural cutoff would split a tool_use/tool_result pair,
// summarizeMessages must either adjust the cutoff or return unchanged.
func TestSummarizeMessages_DoesNotSplitToolPair(t *testing.T) {
	t.Parallel()

	// Mock summarizer that returns a tiny summary.
	sumServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ChatCompletionResponse{
			ID: "sum-pair", Model: "summ-model",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"summary"`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer sumServer.Close()

	summarizer := &ResolvedUpstream{
		Cfg:    UpstreamCfg{Name: "sum", Target: sumServer.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "summ-model"}},
		Client: http.DefaultClient,
	}

	// Conversation:
	//  0: user "hi"
	//  1: assistant tool_use t1
	//  2: user tool_result t1
	//  3: assistant "ok"
	//  4: user "now read another"
	//  5: assistant tool_use t2  ← would be split with keepRecent=2
	//  6: user tool_result t2    ← recent[0] — orphan if cutoff=5
	//  7: assistant "done"
	msgs := []AnthropicMsg{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"x"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`"now read another"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t2","name":"R","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t2","content":"y"}]`)},
		{Role: "assistant", Content: json.RawMessage(`"done"`)},
	}
	req := &MessagesRequest{Messages: msgs}

	result, err := summarizeMessages(context.Background(), req, summarizer, 2, silentLogger(), nil)
	if err != nil {
		t.Fatalf("summarizeMessages: %v", err)
	}

	// Walk result and verify every tool_result has a matching tool_use
	// earlier in the slice.
	seenUseIDs := make(map[string]bool)
	for i, m := range result {
		for _, id := range extractToolUseIDs(m) {
			seenUseIDs[id] = true
		}
		for _, id := range extractToolResultIDs(m) {
			if !seenUseIDs[id] {
				t.Fatalf("result[%d] has orphan tool_result for id %q — summarizer split a tool pair", i, id)
			}
		}
	}
}
