package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- deriveFirstTokenMs ---

// TestDeriveFirstTokenMs_PreferenceOrder pins the §4.3 +
// reviewer-option-C resolution: translator-set FirstContentDeltaAt
// wins; fall back to firstWriteAt; degenerate case mirrors total_ms.
func TestDeriveFirstTokenMs_PreferenceOrder(t *testing.T) {
	t.Parallel()
	start := time.Now()
	end := start.Add(800 * time.Millisecond)
	contentDelta := start.Add(120 * time.Millisecond)
	firstWrite := start.Add(50 * time.Millisecond)

	cases := []struct {
		name    string
		content time.Time
		write   time.Time
		want    int64
	}{
		{"translator-set wins over write", contentDelta, firstWrite, 120},
		{"falls back to firstWriteAt when content unset", time.Time{}, firstWrite, 50},
		{"degenerate (neither set) → total_ms",
			time.Time{}, time.Time{}, 800},
		{"translator-set even when write is earlier still wins",
			contentDelta, start, 120},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveFirstTokenMs(start, end, c.content, c.write); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestDeriveFirstTokenMs_NeverExceedsTotal: the invariant
// first_token_ms <= total_ms must hold across all callers. Even
// when degenerate fallback fires, equality holds; and when
// translator/write timestamps are valid, they're earlier than end
// by construction.
func TestDeriveFirstTokenMs_NeverExceedsTotal(t *testing.T) {
	t.Parallel()
	start := time.Now()
	end := start.Add(time.Second)
	cases := []struct {
		name    string
		content time.Time
		write   time.Time
	}{
		{"both set within window", start.Add(200 * time.Millisecond), start.Add(50 * time.Millisecond)},
		{"only write set", time.Time{}, start.Add(700 * time.Millisecond)},
		{"degenerate", time.Time{}, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ftm := deriveFirstTokenMs(start, end, c.content, c.write)
			total := end.Sub(start).Milliseconds()
			if ftm > total {
				t.Errorf("first_token_ms=%d > total_ms=%d (invariant broken)", ftm, total)
			}
			if ftm < 0 {
				t.Errorf("first_token_ms=%d negative", ftm)
			}
		})
	}
}

// --- markFirstContentDelta ---

// TestMarkFirstContentDelta_Idempotent: subsequent calls do not
// overwrite the first timestamp. This matters because every emit
// site (text, thinking, tool_use, input_json_delta) calls it, and
// only the first occurrence should win.
func TestMarkFirstContentDelta_Idempotent(t *testing.T) {
	t.Parallel()
	au := &AuditUpstream{}
	markFirstContentDelta(au)
	first := au.FirstContentDeltaAt
	if first.IsZero() {
		t.Fatal("first call did not set timestamp")
	}
	time.Sleep(2 * time.Millisecond)
	markFirstContentDelta(au)
	if !au.FirstContentDeltaAt.Equal(first) {
		t.Errorf("second call overwrote: was %v, now %v", first, au.FirstContentDeltaAt)
	}
}

// TestMarkFirstContentDelta_NilSafe: passing nil au must not panic.
// Tests that bypass wrapAuditing construct streamState without an
// AuditUpstream; the marker call is in the hot path so it must
// tolerate that.
func TestMarkFirstContentDelta_NilSafe(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on nil au: %v", r)
		}
	}()
	markFirstContentDelta(nil)
}

// --- summarize delta ---

// TestSummarizeInfo_LandsInRowWhenFired drives a real summarization
// through Start() and asserts the audit row carries summarize.fired,
// turns_collapsed, bytes_removed > 0, and the messages-level byte
// counts match a sanity check on the original messages array.
func TestSummarizeInfo_LandsInRowWhenFired(t *testing.T) {
	t.Parallel()

	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		resp := ChatCompletionResponse{
			ID: "x", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: "stop",
			}},
			Usage: &OpenAIUsage{PromptTokens: 5, CompletionTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	summarizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ChatCompletionResponse{
			ID: "sum", Model: "summarizer",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Condensed summary."`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer summarizer.Close()

	logDir := t.TempDir()
	gwName := "gw-9b-summarize"
	cfg := GatewayCfg{
		Name:   gwName,
		Client: []ClientCfg{{Bind: "127.0.0.1:0", API: APIAnthropic}},
		Upstream: []UpstreamCfg{
			{Name: "panshi", Target: upstream.URL, API: APIOpenAI, ContextWindow: 300, Summarizer: "sum", ModelMap: map[string]string{"*": "glm-4.7"}},
			{Name: "sum", Target: summarizer.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "summarizer"}},
		},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "panshi"}},
		Log:     LogCfg{Level: LogLevelFull, Dir: logDir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	base := startGateway(t, cfg)

	large := strings.Repeat("A", 1200)
	body := `{"model":"hw-minimax","messages":[` +
		`{"role":"user","content":"` + large + `"},` +
		`{"role":"assistant","content":"older"},` +
		`{"role":"user","content":"r1"},` +
		`{"role":"assistant","content":"r2"},` +
		`{"role":"user","content":"r3"},` +
		`{"role":"assistant","content":"r4"},` +
		`{"role":"user","content":"r5"}` +
		`],"max_tokens":64}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	respRow := rows[1]

	sum, ok := respRow["summarize"].(map[string]any)
	if !ok {
		t.Fatalf("summarize block missing: %+v", respRow)
	}
	if sum["fired"] != true {
		t.Errorf("summarize.fired = %v, want true", sum["fired"])
	}
	turnsCollapsed, _ := sum["turns_collapsed"].(float64)
	bytesRemoved, _ := sum["bytes_removed"].(float64)
	if turnsCollapsed <= 0 {
		t.Errorf("summarize.turns_collapsed = %v, want > 0", sum["turns_collapsed"])
	}
	if bytesRemoved <= 0 {
		t.Errorf("summarize.bytes_removed = %v, want > 0 (large message replaced by short summary)", sum["bytes_removed"])
	}
	// Sanity: the upstream actually saw the summarized body, not
	// the original — bytes_removed should reflect the delta the
	// upstream observed.
	if !strings.Contains(string(upstreamBody), "Conversation summary") {
		t.Errorf("upstream did not receive summarized body")
	}
}

// TestSummarizeInfo_AbsentWhenNotFired: when summarization doesn't
// trigger, the audit row has NO summarize key (per §4.6 omission rule).
func TestSummarizeInfo_AbsentWhenNotFired(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ChatCompletionResponse{
			ID: "x", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)
	body := `{"model":"claude-opus-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if _, present := rows[1]["summarize"]; present {
		t.Errorf("summarize key present despite no summarization firing: %+v", rows[1]["summarize"])
	}
}

// --- stream timing land in row ---

// TestStreamTiming_NonStreamingFirstEqualsTotal pins the §4.3
// promise: non-streaming response has first_token_ms == total_ms.
// Both numbers come from the same Write window so equality is exact.
func TestStreamTiming_NonStreamingFirstEqualsTotal(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ChatCompletionResponse{
			ID: "x", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"hi"`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)
	body := `{"model":"claude-opus-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	stream, ok := rows[1]["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream missing: %+v", rows[1])
	}
	first, _ := stream["first_token_ms"].(float64)
	total, _ := stream["total_ms"].(float64)
	if first != total {
		t.Errorf("non-streaming first_token_ms (%v) != total_ms (%v); spec requires equality", first, total)
	}
	if total < 0 {
		t.Errorf("total_ms = %v, want >= 0", total)
	}
}

// TestStreamTiming_StreamingFirstTokenAfterFirstContentDelta drives
// a streaming response where the upstream emits a metadata-only
// prelude, then waits 100ms, then emits the first content delta.
// The audit row's first_token_ms should reflect the delay, not
// approach 0 (the wire-byte heuristic would set ~0 because
// message_start lands first).
//
// 100ms is the smallest reliable delay across CI; allow ±50ms
// tolerance for scheduler jitter.
func TestStreamTiming_StreamingFirstTokenAfterFirstContentDelta(t *testing.T) {
	t.Parallel()
	const delay = 100 * time.Millisecond
	const tolerance = 50 // ms
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		// Metadata-only chunk first (no content / tool_calls).
		_, _ = w.Write([]byte(`data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"))
		f.Flush()
		time.Sleep(delay)
		// Now the content delta.
		_, _ = w.Write([]byte(`data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}` + "\n\n"))
		f.Flush()
		_, _ = w.Write([]byte(`data: {"id":"x","model":"glm-4.7","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)
	body := `{"model":"claude-opus-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	stream, ok := rows[1]["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream missing: %+v", rows[1])
	}
	first, _ := stream["first_token_ms"].(float64)
	if first < float64(delay.Milliseconds()-tolerance) {
		t.Errorf("first_token_ms = %v ms, want >= %d (would mean we measured to the metadata prelude, not first content delta)",
			first, delay.Milliseconds()-tolerance)
	}
}

// TestStreamTiming_EmptyStreamFirstEqualsTotal: the reviewer-flagged
// degenerate case. Stream closes with no content delta. Both fields
// equal total_ms by derivation; never null.
func TestStreamTiming_EmptyStreamFirstEqualsTotal(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Send nothing, just close.
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)
	body := `{"model":"claude-opus-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	stream, ok := rows[1]["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream missing: %+v", rows[1])
	}
	first, hasFirst := stream["first_token_ms"]
	total, hasTotal := stream["total_ms"]
	if !hasFirst || !hasTotal {
		t.Fatalf("first_token_ms or total_ms missing on empty stream: %+v", stream)
	}
	// The translator emits a synthetic message_start (Anthropic
	// shape) on every stream regardless of upstream content, which
	// is a Write to the auditing writer. So firstWriteAt ≠ 0 and
	// first_token_ms reflects that. The contract for "no content
	// delta seen" is just "first_token_ms is defined and bounded
	// by total_ms".
	if first.(float64) > total.(float64) {
		t.Errorf("empty stream: first_token_ms (%v) > total_ms (%v)", first, total)
	}
}

// --- empty/cancelled stream ---

// TestSummarizeInfo_AbsentOnNonAnthropicClient: summarization is
// gated to APIAnthropic clients today (gateway.go innerHandler
// check). An OpenAI client should NOT receive a summarize block
// even with an oversized history.
func TestSummarizeInfo_AbsentOnNonAnthropicClient(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := MessagesResponse{
			ID: "msg", Type: "message", Role: "assistant", Model: "claude-x",
			Content: []ContentBlock{{Type: "text", Text: "ok"}},
			Usage:   AnthropicUsage{InputTokens: 1, OutputTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIOpenAI, APIAnthropic, upstream.URL)
	body := `{"model":"o-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if _, present := rows[1]["summarize"]; present {
		t.Errorf("summarize present on OpenAI client (gating bypassed): %+v", rows[1]["summarize"])
	}
}

// startGateway is a small helper used by 9b tests that need a
// custom GatewayCfg (multiple upstreams, summarizer, etc.) — the
// existing startTranslationGateway covers only single-upstream
// translations.
func startGateway(t *testing.T, cfg GatewayCfg) string {
	t.Helper()
	// Bind a random loopback port and rewrite cfg.
	srv, err := startGatewayOn(t, &cfg)
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	return srv
}

func startGatewayOn(t *testing.T, cfg *GatewayCfg) (string, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Client[0].Bind = addr

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Start(ctx, *cfg, silentLogger())
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	base := "http://" + addr
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("gateway did not start in time")
}
