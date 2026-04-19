package gateway

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestReassembleSSE_AnthropicTextAndUsage(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_abc","model":"claude-opus-4-6","usage":{"input_tokens":17,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil {
		t.Fatal("summary is nil")
	}
	if got.Content != "Hello, world!" {
		t.Errorf("content = %q, want %q", got.Content, "Hello, world!")
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", got.StopReason)
	}
	if got.MessageID != "msg_abc" {
		t.Errorf("message_id = %q", got.MessageID)
	}
	if got.Model != "claude-opus-4-6" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Usage == nil || got.Usage.InputTokens != 17 || got.Usage.OutputTokens != 42 {
		t.Errorf("usage = %+v, want in=17 out=42", got.Usage)
	}
	if got.Events < 7 {
		t.Errorf("events = %d, want >=7", got.Events)
	}
}

func TestReassembleSSE_AnthropicToolUse(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ation\":\"Ankara\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil || len(got.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", got)
	}
	call := got.ToolCalls[0]
	if call.ID != "toolu_123" || call.Name != "get_weather" {
		t.Errorf("tool call id/name = %q/%q", call.ID, call.Name)
	}
	var args map[string]string
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("args not valid JSON: %v; got %s", err, call.Args)
	}
	if args["location"] != "Ankara" {
		t.Errorf("reassembled args = %v", args)
	}
}

func TestReassembleSSE_AnthropicThinking(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me see... "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"the answer is 42."}}`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil || got.Thinking != "Let me see... the answer is 42." {
		t.Errorf("thinking = %q", got.Thinking)
	}
}

func TestReassembleSSE_AnthropicError(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude-opus-4-6"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"server is overloaded"}}`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil || len(got.Errors) != 1 {
		t.Fatalf("errors = %+v", got)
	}
	if !strings.Contains(got.Errors[0], "overloaded") {
		t.Errorf("error text = %q", got.Errors[0])
	}
}

func TestReassembleSSE_OpenAI(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-xyz","model":"gpt-4o","choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-xyz","model":"gpt-4o","choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-xyz","model":"gpt-4o","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIOpenAI)
	if got == nil {
		t.Fatal("nil summary")
	}
	if got.Content != "Hello world" {
		t.Errorf("content = %q", got.Content)
	}
	if got.StopReason != "stop" {
		t.Errorf("stop_reason = %q", got.StopReason)
	}
	if got.MessageID != "chatcmpl-xyz" || got.Model != "gpt-4o" {
		t.Errorf("id/model = %q/%q", got.MessageID, got.Model)
	}
	if got.Usage == nil || got.Usage.InputTokens != 9 || got.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestReassembleSSE_OpenAIToolCalls(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"get_weather","arguments":"{\"loc"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"Ankara\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got := reassembleSSE([]byte(stream), APIOpenAI)
	if got == nil || len(got.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", got)
	}
	call := got.ToolCalls[0]
	if call.ID != "call_abc" || call.Name != "get_weather" {
		t.Errorf("id/name = %q/%q", call.ID, call.Name)
	}
	var args map[string]string
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("args invalid: %v; got %s", err, call.Args)
	}
	if args["location"] != "Ankara" {
		t.Errorf("args = %v", args)
	}
}

func TestReassembleSSE_TruncatedStream(t *testing.T) {
	t.Parallel()
	// Stream cut off mid-text_delta — client cancel or network drop.
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_cut","model":"claude-opus-4-6"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"reply`
	// Note: no trailing newline/blank line — simulates cut.
	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil {
		t.Fatal("nil summary on truncated stream")
	}
	if !strings.HasPrefix(got.Content, "partial ") {
		t.Errorf("content = %q, want prefix 'partial '", got.Content)
	}
	if got.MessageID != "msg_cut" {
		t.Errorf("message_id lost on truncation: %q", got.MessageID)
	}
}

// TestReassembleSSE_AnthropicCacheTokens verifies that prompt-cache fields
// surface from message_start and that message_delta updates carry them through
// when the upstream re-emits the cache counters mid-stream.
func TestReassembleSSE_AnthropicCacheTokens(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude-opus-4-6","usage":{"input_tokens":50,"output_tokens":1,"cache_creation_input_tokens":2000,"cache_read_input_tokens":7000}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42,"cache_read_input_tokens":7100}}`,
		``,
	}, "\n")
	got := reassembleSSE([]byte(stream), APIAnthropic)
	if got == nil || got.Usage == nil {
		t.Fatal("nil summary or usage")
	}
	if got.Usage.InputTokens != 50 || got.Usage.OutputTokens != 42 {
		t.Errorf("input/output = %d/%d", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.CacheCreationInputTokens != 2000 {
		t.Errorf("cache_creation = %d, want 2000", got.Usage.CacheCreationInputTokens)
	}
	// message_delta should overwrite when non-zero (cache window grew mid-stream).
	if got.Usage.CacheReadInputTokens != 7100 {
		t.Errorf("cache_read = %d, want 7100", got.Usage.CacheReadInputTokens)
	}
}

// TestReassembleSSE_OpenAIDetailTokens verifies that prompt_tokens_details and
// completion_tokens_details surface when the upstream emits include_usage with
// reasoning details (o-series) and cached prompt fragments.
func TestReassembleSSE_OpenAIDetailTokens(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"o1-mini","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","model":"o1-mini","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":25}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got := reassembleSSE([]byte(stream), APIOpenAI)
	if got == nil || got.Usage == nil {
		t.Fatal("nil summary or usage")
	}
	if got.Usage.InputTokens != 120 || got.Usage.OutputTokens != 80 {
		t.Errorf("in/out = %d/%d", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadInputTokens != 40 {
		t.Errorf("cached = %d, want 40", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.ReasoningTokens != 25 {
		t.Errorf("reasoning = %d, want 25", got.Usage.ReasoningTokens)
	}
}

func TestReassembleSSE_EmptyBody(t *testing.T) {
	t.Parallel()
	if got := reassembleSSE(nil, APIAnthropic); got != nil {
		t.Errorf("nil body should return nil, got %+v", got)
	}
	if got := reassembleSSE([]byte(""), APIOpenAI); got != nil {
		t.Errorf("empty body should return nil, got %+v", got)
	}
}

// --- Property tests ---
//
// These pin that reassembly is invariant under the two degrees of freedom
// upstreams actually exercise: unpredictable chunk boundaries inside the
// text/JSON stream, and variable whitespace between SSE events. A counter-
// example here would mean a production stream could produce a different
// stream_summary depending on how the upstream happened to flush buffers.

// randomSplit returns a random partition of s into `parts` non-empty chunks
// (or as many as fit when rune count < parts). Splits are placed at rune
// boundaries so multi-byte UTF-8 sequences are never cut mid-byte; real
// streaming producers respect rune boundaries too.
func randomSplit(rng *rand.Rand, s string, parts int) []string {
	runes := []rune(s)
	if parts <= 1 || len(runes) <= 1 {
		return []string{s}
	}
	if parts > len(runes) {
		parts = len(runes)
	}
	cuts := make([]int, 0, parts-1)
	seen := map[int]bool{}
	for len(cuts) < parts-1 {
		c := 1 + rng.Intn(len(runes)-1)
		if seen[c] {
			continue
		}
		seen[c] = true
		cuts = append(cuts, c)
	}
	sort.Ints(cuts)
	out := make([]string, 0, parts)
	prev := 0
	for _, c := range cuts {
		out = append(out, string(runes[prev:c]))
		prev = c
	}
	out = append(out, string(runes[prev:]))
	return out
}

func TestReassembleSSE_Anthropic_TextChunkingInvariance(t *testing.T) {
	t.Parallel()
	const golden = "The quick brown fox jumps over 12 lazy dogs. Also: UTF-8 snowman ☃ and emoji 🦊!"
	// Seeds picked for diversity; reproducible.
	for _, seed := range []int64{1, 7, 42, 99, 2026} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed))
			parts := 1 + rng.Intn(12)
			chunks := randomSplit(rng, golden, parts)

			var b strings.Builder
			b.WriteString("event: message_start\n")
			b.WriteString(`data: {"type":"message_start","message":{"id":"msg_x","model":"m"}}`)
			b.WriteString("\n\n")
			b.WriteString("event: content_block_start\n")
			b.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			b.WriteString("\n\n")
			for _, c := range chunks {
				enc, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				b.WriteString("event: content_block_delta\n")
				fmt.Fprintf(&b,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`,
					enc)
				b.WriteString("\n\n")
			}
			b.WriteString("event: message_delta\n")
			b.WriteString(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
			b.WriteString("\n\n")

			got := reassembleSSE([]byte(b.String()), APIAnthropic)
			if got == nil {
				t.Fatal("nil summary")
			}
			if got.Content != golden {
				t.Errorf("content mismatch (parts=%d)\ngot  %q\nwant %q", parts, got.Content, golden)
			}
			if got.StopReason != "end_turn" {
				t.Errorf("stop_reason = %q", got.StopReason)
			}
		})
	}
}

func TestReassembleSSE_Anthropic_ToolArgsChunkingInvariance(t *testing.T) {
	t.Parallel()
	// Non-trivial JSON so splits at different offsets exercise the partial_json
	// buffer. After reassembly, args must parse back to this exact object.
	want := map[string]any{
		"query":   "weather in Ankara",
		"max":     int64(5),
		"flags":   []any{"verbose", "json"},
		"deep":    map[string]any{"a": "b", "n": float64(3.14)},
		"unicode": "snowman ☃",
	}
	wantBytes, _ := json.Marshal(want)
	wantJSON := string(wantBytes)

	for _, seed := range []int64{3, 17, 51, 256, 2026} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed))
			parts := 1 + rng.Intn(10)
			chunks := randomSplit(rng, wantJSON, parts)

			var b strings.Builder
			b.WriteString("event: content_block_start\n")
			b.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tid_1","name":"search"}}`)
			b.WriteString("\n\n")
			for _, c := range chunks {
				enc, _ := json.Marshal(c)
				b.WriteString("event: content_block_delta\n")
				fmt.Fprintf(&b,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`,
					enc)
				b.WriteString("\n\n")
			}
			b.WriteString("event: content_block_stop\n")
			b.WriteString(`data: {"type":"content_block_stop","index":0}`)
			b.WriteString("\n\n")

			got := reassembleSSE([]byte(b.String()), APIAnthropic)
			if got == nil || len(got.ToolCalls) != 1 {
				t.Fatalf("summary = %+v", got)
			}
			tc := got.ToolCalls[0]
			if tc.ID != "tid_1" || tc.Name != "search" {
				t.Errorf("tool_call id/name = %q/%q", tc.ID, tc.Name)
			}
			var gotArgs map[string]any
			if err := json.Unmarshal(tc.Args, &gotArgs); err != nil {
				t.Fatalf("args not valid JSON after reassembly (parts=%d): %v\nargs=%s", parts, err, tc.Args)
			}
			gotCanon, _ := json.Marshal(gotArgs)
			wantCanon, _ := json.Marshal(want)
			if string(gotCanon) != string(wantCanon) {
				t.Errorf("args mismatch (parts=%d)\ngot  %s\nwant %s", parts, gotCanon, wantCanon)
			}
		})
	}
}

func TestReassembleSSE_OpenAI_ToolArgsChunkingInvariance(t *testing.T) {
	t.Parallel()
	want := map[string]any{"q": "hello", "n": float64(42)}
	wantBytes, _ := json.Marshal(want)
	wantJSON := string(wantBytes)

	for _, seed := range []int64{5, 11, 88, 777, 2026} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed))
			parts := 1 + rng.Intn(8)
			chunks := randomSplit(rng, wantJSON, parts)

			var b strings.Builder
			// First chunk carries the id/name; subsequent chunks append args.
			first := true
			for _, c := range chunks {
				var chunk string
				enc, _ := json.Marshal(c)
				if first {
					chunk = fmt.Sprintf(
						`data: {"id":"cc_1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":%s}}]}}]}`,
						enc)
					first = false
				} else {
					chunk = fmt.Sprintf(
						`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`,
						enc)
				}
				b.WriteString(chunk)
				b.WriteString("\n\n")
			}
			b.WriteString(`data: {"choices":[{"finish_reason":"tool_calls"}]}`)
			b.WriteString("\n\n")
			b.WriteString("data: [DONE]\n\n")

			got := reassembleSSE([]byte(b.String()), APIOpenAI)
			if got == nil || len(got.ToolCalls) != 1 {
				t.Fatalf("summary = %+v", got)
			}
			tc := got.ToolCalls[0]
			if tc.ID != "call_1" || tc.Name != "f" {
				t.Errorf("tool_call id/name = %q/%q", tc.ID, tc.Name)
			}
			var gotArgs map[string]any
			if err := json.Unmarshal(tc.Args, &gotArgs); err != nil {
				t.Fatalf("args not valid JSON (parts=%d): %v\nraw=%s", parts, err, tc.Args)
			}
			gotCanon, _ := json.Marshal(gotArgs)
			wantCanon, _ := json.Marshal(want)
			if string(gotCanon) != string(wantCanon) {
				t.Errorf("args mismatch (parts=%d)\ngot  %s\nwant %s", parts, gotCanon, wantCanon)
			}
		})
	}
}
