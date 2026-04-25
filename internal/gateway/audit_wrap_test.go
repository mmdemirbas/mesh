package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTranslationGateway spins up a Start()-backed gateway for translation
// directions (a2o/o2a) with the audit log enabled under t.TempDir().
func startTranslationGateway(t *testing.T, clientAPI, upstreamAPI, upstreamURL string) (base string, gwName string, logDir string) {
	t.Helper()
	logDir = t.TempDir()
	gwName = "gw-" + strings.ReplaceAll(t.Name(), "/", "_")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := GatewayCfg{
		Name: gwName,
		Client: []ClientCfg{
			{Bind: addr, API: clientAPI},
		},
		Upstream: []UpstreamCfg{
			{Name: "default", Target: upstreamURL, API: upstreamAPI},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"*"}, UpstreamName: "default"},
		},
		Log: LogCfg{Level: LogLevelFull, Dir: logDir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Start(ctx, cfg, silentLogger())
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	base = "http://" + addr
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway did not start in time")
	return
}

func TestTranslationAudit_A2O_NonStreaming(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatCompletionResponse{
			ID: "chatcmpl-audit", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"audit-me"`)},
				FinishReason: "stop",
			}},
			Usage: &OpenAIUsage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

	body := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var anth MessagesResponse
	if err := json.Unmarshal(got, &anth); err != nil {
		t.Fatalf("parse client response: %v; body=%s", err, got)
	}

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2; rows=%+v", len(rows), rows)
	}
	req, respRow := rows[0], rows[1]
	if req["direction"] != "a2o" {
		t.Errorf("direction = %v, want a2o", req["direction"])
	}
	if req["model"] != "claude-opus-4-6" {
		t.Errorf("req model = %v", req["model"])
	}
	// Session id and turn index must land in the request row so the UI can
	// group conversations and show prompt growth.
	if sid, _ := req["session_id"].(string); len(sid) != sessionIDLen {
		t.Errorf("session_id missing or wrong length: %q", sid)
	}
	if ti, _ := req["turn_index"].(float64); ti != 1 {
		t.Errorf("turn_index = %v, want 1", ti)
	}
	if respRow["status"].(float64) != 200 {
		t.Errorf("status = %v", respRow["status"])
	}
	// Translation produces Anthropic-format response; parseUsage with
	// clientAPI=anthropic should pick up the translated token counts.
	usage, ok := respRow["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %+v", respRow)
	}
	if usage["input_tokens"].(float64) != 11 || usage["output_tokens"].(float64) != 3 {
		t.Errorf("translated usage = %v, want in=11 out=3", usage)
	}
}

func TestTranslationAudit_A2O_NonStreamingUpstreamErrorDetails(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

	body := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var anth AnthropicErrorResponse
	if err := json.Unmarshal(got, &anth); err != nil {
		t.Fatalf("parse anthropic error: %v; body=%s", err, got)
	}
	if !strings.Contains(anth.Error.Message, "Not Found") {
		t.Fatalf("client error message = %q, want upstream detail", anth.Error.Message)
	}

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	respRow := rows[1]
	if respRow["upstream_resp"] == nil {
		t.Fatalf("upstream_resp missing from audit row: %+v", respRow)
	}
}

func TestTranslationAudit_O2A_NonStreamingUpstreamErrorDetails(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"too fast"}}`))
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIOpenAI, APIAnthropic, upstream.URL)

	body := `{"model":"o-model","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var oai OpenAIErrorResponse
	if err := json.Unmarshal(got, &oai); err != nil {
		t.Fatalf("parse openai error: %v; body=%s", err, got)
	}
	if !strings.Contains(oai.Error.Message, "too fast") {
		t.Fatalf("client error message = %q, want upstream detail", oai.Error.Message)
	}

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	respRow := rows[1]
	if respRow["upstream_resp"] == nil {
		t.Fatalf("upstream_resp missing from audit row: %+v", respRow)
	}
}

func TestTranslationAudit_A2O_SummarizationPreservesOriginalAuditBody(t *testing.T) {
	t.Parallel()

	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		resp := ChatCompletionResponse{
			ID: "chatcmpl-sum", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"summarized ok"`)},
				FinishReason: "stop",
			}},
			Usage: &OpenAIUsage{PromptTokens: 42, CompletionTokens: 7, TotalTokens: 49},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	summarizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatCompletionResponse{
			ID: "chatcmpl-summary", Model: "gemini-2.0-flash",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"Condensed history for the active coding task."`)},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer summarizer.Close()

	logDir := t.TempDir()
	gwName := "gw-" + strings.ReplaceAll(t.Name(), "/", "_")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := GatewayCfg{
		Name:   gwName,
		Client: []ClientCfg{{Bind: addr, API: APIAnthropic}},
		Upstream: []UpstreamCfg{
			{Name: "panshi", Target: upstream.URL, API: APIOpenAI, ContextWindow: 300, Summarizer: "sum", ModelMap: map[string]string{"*": "glm-4.7"}},
			{Name: "sum", Target: summarizer.URL, API: APIOpenAI, ModelMap: map[string]string{"*": "gemini-2.0-flash"}},
		},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "panshi"}},
		Log:     LogCfg{Level: LogLevelFull, Dir: logDir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Start(ctx, cfg, silentLogger())
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
	started := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				started = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("gateway did not start in time")
	}

	large := strings.Repeat("A", 1200)
	body := `{"model":"hw-minimax","messages":[{"role":"user","content":"` + large + `"},{"role":"assistant","content":"older reply"},{"role":"user","content":"recent-1"},{"role":"assistant","content":"recent-2"},{"role":"user","content":"recent-3"},{"role":"assistant","content":"recent-4"},{"role":"user","content":"recent-5"}],"max_tokens":64}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, got)
	}

	var anth MessagesResponse
	if err := json.Unmarshal(got, &anth); err != nil {
		t.Fatalf("parse client response: %v; body=%s", err, got)
	}

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2; rows=%+v", len(rows), rows)
	}
	reqRow, respRow := rows[0], rows[1]
	reqJSON, err := json.Marshal(reqRow["body"])
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	if !strings.Contains(string(reqJSON), large) {
		t.Fatalf("request audit body does not contain original oversized prompt")
	}

	upReqJSON, err := json.Marshal(respRow["upstream_req"])
	if err != nil {
		t.Fatalf("marshal upstream_req: %v", err)
	}
	if respRow["summarized"] != true {
		t.Fatalf("summarized = %v, want true", respRow["summarized"])
	}
	if got := respRow["context_window_tokens"]; got != float64(300) {
		t.Fatalf("context_window_tokens = %v, want 300", got)
	}
	orig, ok := respRow["original_input_tokens_estimate"].(float64)
	if !ok || orig <= 300 {
		t.Fatalf("original_input_tokens_estimate = %v, want > 300", respRow["original_input_tokens_estimate"])
	}
	eff, ok := respRow["effective_input_tokens_estimate"].(float64)
	if !ok || eff > 300 {
		t.Fatalf("effective_input_tokens_estimate = %v, want <= 300", respRow["effective_input_tokens_estimate"])
	}
	if !strings.Contains(string(upReqJSON), "Conversation summary") {
		t.Fatalf("upstream_req does not contain summarized conversation: %s", upReqJSON)
	}
	if strings.Contains(string(upReqJSON), large) {
		t.Fatalf("upstream_req still contains original oversized prompt")
	}
	if !strings.Contains(string(upstreamBody), "Conversation summary") {
		t.Fatalf("upstream body was not summarized: %s", upstreamBody)
	}
}

func TestTranslationAudit_A2O_StreamingReassembly(t *testing.T) {
	t.Parallel()
	// Upstream returns OpenAI SSE chunks — gateway will translate to Anthropic
	// SSE and emit to client. Audit tees the emitted (Anthropic) bytes.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-s","model":"glm-4.7","choices":[{"delta":{"content":"stream "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-s","model":"glm-4.7","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-s","model":"glm-4.7","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			f.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

	body := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	respRow := rows[1]
	summary, ok := respRow["stream_summary"].(map[string]any)
	if !ok {
		t.Fatalf("stream_summary missing: %+v", respRow)
	}
	// Translation produces Anthropic-format SSE; reassembler parses it and
	// concatenates text deltas.
	if summary["content"] != "stream ok" {
		t.Errorf("reassembled content = %v, want %q", summary["content"], "stream ok")
	}
	if summary["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v (expected mapped from OpenAI 'stop')", summary["stop_reason"])
	}
}

func TestTranslationAudit_A2O_StreamingUpstreamError(t *testing.T) {
	t.Parallel()
	// Upstream returns 400 with an error body for a streaming request.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"context window exceeded","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if len(rows) < 2 {
		t.Fatalf("audit rows = %d, want >= 2", len(rows))
	}
	respRow := rows[1]
	if respRow["status"].(float64) != 400 {
		t.Errorf("status = %v, want 400", respRow["status"])
	}
	// The upstream error body must be captured in upstream_resp.
	upResp, ok := respRow["upstream_resp"]
	if !ok || upResp == nil {
		t.Fatalf("upstream_resp missing from audit row: %+v", respRow)
	}
	upRespMap, ok := upResp.(map[string]any)
	if !ok {
		t.Fatalf("upstream_resp not a JSON object: %T %v", upResp, upResp)
	}
	errObj, _ := upRespMap["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "context window exceeded" {
		t.Errorf("upstream_resp error = %v, want context window exceeded message", upRespMap)
	}
}

func TestTranslationAudit_O2A_StreamingUpstreamError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"too fast"}}`))
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIOpenAI, APIAnthropic, upstream.URL)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	if len(rows) < 2 {
		t.Fatalf("audit rows = %d, want >= 2", len(rows))
	}
	respRow := rows[1]
	// The upstream error body must be captured in upstream_resp.
	upResp, ok := respRow["upstream_resp"]
	if !ok || upResp == nil {
		t.Fatalf("upstream_resp missing from audit row: %+v", respRow)
	}
}

// TestTranslationAudit_TopToolResultLandsInRow drives a request
// containing two tool_result blocks of distinct sizes and asserts
// the audit response row carries top_tool_result with the larger
// block's metadata. End-to-end check that 7b₁ wiring populates the
// row (not just the analyzeRequest function).
func TestTranslationAudit_TopToolResultLandsInRow(t *testing.T) {
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

	// 250-byte and 30-byte tool_results, with the assistant's
	// tool_use blocks earlier so the linkage works.
	smallContent := strings.Repeat("s", 30)
	bigContent := strings.Repeat("B", 250)
	body := `{"model":"claude-opus-4-6","max_tokens":10,"messages":[` +
		`{"role":"user","content":"start"},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"u_small","name":"Read","input":{"file_path":"/a"}},` +
		`{"type":"tool_use","id":"u_big","name":"Grep","input":{"pattern":"x","path":"/b"}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"u_small","content":"` + smallContent + `"},` +
		`{"type":"tool_result","tool_use_id":"u_big","content":"` + bigContent + `"}` +
		`]}` +
		`]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	respRow := rows[1]

	top, ok := respRow["top_tool_result"].(map[string]any)
	if !ok {
		t.Fatalf("top_tool_result missing from audit row: %+v", respRow)
	}
	if got := top["bytes"]; got != float64(len(bigContent)) {
		t.Errorf("top_tool_result.bytes = %v, want %d", got, len(bigContent))
	}
	if got := top["tool_use_id"]; got != "u_big" {
		t.Errorf("top_tool_result.tool_use_id = %v, want u_big", got)
	}
	if got := top["tool_name"]; got != "Grep" {
		t.Errorf("top_tool_result.tool_name = %v, want Grep (linked via tool_use_id)", got)
	}
}

// TestTranslationAudit_ResponseBytesAndStreamLandInRow drives a full
// SSE response through Start() and asserts the audit row carries
// the §4.3 response_bytes partition plus stream.terminated.
// Verifies the wiring from sse_reassembly → wrapAuditing → row writer
// end-to-end, not just the unit-test path.
func TestTranslationAudit_ResponseBytesAndStreamLandInRow(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		// OpenAI-shape SSE; the gateway translates to Anthropic for
		// the client. We assert on the captured CLIENT-side bytes
		// (Anthropic) in the audit row.
		chunks := []string{
			`{"id":"x","model":"glm-4.7","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
			`{"id":"x","model":"glm-4.7","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			f.Flush()
		}
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
	respRow := rows[1]

	rb, ok := respRow["response_bytes"].(map[string]any)
	if !ok {
		t.Fatalf("response_bytes missing from audit row: %+v", respRow)
	}
	total, _ := rb["total"].(float64)
	text, _ := rb["text"].(float64)
	thinking, _ := rb["thinking"].(float64)
	toolCalls, _ := rb["tool_calls"].(float64)
	other, _ := rb["other"].(float64)
	if total <= 0 {
		t.Errorf("response_bytes.total = %v, want > 0", total)
	}
	if sum := text + thinking + toolCalls + other; int(sum) != int(total) {
		t.Errorf("response_bytes partition broken: text=%v thinking=%v tool_calls=%v other=%v sum=%v total=%v",
			text, thinking, toolCalls, other, sum, total)
	}
	if text < 2 {
		t.Errorf("response_bytes.text = %v, want at least 2 ('hi')", text)
	}

	stream, ok := respRow["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream block missing: %+v", respRow)
	}
	if stream["terminated"] != "normal" {
		t.Errorf("stream.terminated = %v, want normal (gateway emits message_stop)", stream["terminated"])
	}
}

// TestTranslationAudit_NonStreamingResponseBytesAndStreamLandInRow
// pins the parseResponseBytes path: a non-streaming response must
// also produce response_bytes with partition closure, and
// stream.terminated must be "normal".
func TestTranslationAudit_NonStreamingResponseBytesAndStreamLandInRow(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ChatCompletionResponse{
			ID: "x", Model: "glm-4.7",
			Choices: []OpenAIChoice{{
				Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"hello"`)},
				FinishReason: "stop",
			}},
			Usage: &OpenAIUsage{PromptTokens: 5, CompletionTokens: 1},
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
	respRow := rows[1]

	rb, ok := respRow["response_bytes"].(map[string]any)
	if !ok {
		t.Fatalf("non-streaming response_bytes missing: %+v", respRow)
	}
	total, _ := rb["total"].(float64)
	text, _ := rb["text"].(float64)
	thinking, _ := rb["thinking"].(float64)
	toolCalls, _ := rb["tool_calls"].(float64)
	other, _ := rb["other"].(float64)
	if int(text+thinking+toolCalls+other) != int(total) {
		t.Errorf("non-streaming partition broken: %+v", rb)
	}
	if text < 5 {
		t.Errorf("response_bytes.text = %v, want >= 5 ('hello')", text)
	}

	stream, ok := respRow["stream"].(map[string]any)
	if !ok {
		t.Fatalf("non-streaming stream block missing: %+v", respRow)
	}
	if stream["terminated"] != "normal" {
		t.Errorf("non-streaming stream.terminated = %v, want normal", stream["terminated"])
	}
}

// TestTranslationAudit_RepeatReadsLandsInRow drives two consecutive
// gateway requests through Start() with identical session id and
// identical Read(/foo) tool_use, verifying the second request's row
// carries repeat_reads.
func TestTranslationAudit_RepeatReadsLandsInRow(t *testing.T) {
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

	body := `{"model":"claude-opus-4-6","max_tokens":10,"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"u","name":"Read","input":{"file_path":"/foo"}}` +
		`]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":"x"}]}` +
		`]}`

	doRequest := func() {
		req, err := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Mesh-Session", "fixed-session-id")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
	}

	doRequest() // turn 1: no prior history.
	doRequest() // turn 2: should see repeat_reads.

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 4, 2*time.Second)
	// rows are interleaved req/resp/req/resp; resp rows are at
	// indices 1 and 3.
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (two pairs)", len(rows))
	}
	resp1 := rows[1]
	resp2 := rows[3]

	if _, present := resp1["repeat_reads"]; present {
		t.Errorf("turn 1 row carries repeat_reads but should be omitted (no prior history): %+v", resp1["repeat_reads"])
	}
	rr, ok := resp2["repeat_reads"].(map[string]any)
	if !ok {
		t.Fatalf("turn 2 row missing repeat_reads: %+v", resp2)
	}
	if got := rr["count"]; got != float64(1) {
		t.Errorf("repeat_reads.count = %v, want 1", got)
	}
	if got := rr["max_same_path"]; got != float64(2) {
		t.Errorf("repeat_reads.max_same_path = %v, want 2", got)
	}
}

// TestTranslationAudit_TimingMsLandsInRow verifies that every audited
// resp row carries a timing_ms object whose six named segments plus
// other sum to total, and whose total equals stream.total_ms exactly
// (D1 invariant from DESIGN_B1_timing.local.md).
//
// The upstream sleeps for upstreamDelay before responding so the
// integer-millisecond timing values are measurably non-zero — without
// the delay, a localhost round-trip completes in well under 1 ms and
// every segment truncates to 0, which closes the partition trivially
// but proves nothing about whether instrumentation fired.
func TestTranslationAudit_TimingMsLandsInRow(t *testing.T) {
	t.Parallel()
	const upstreamDelay = 30 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Pace the upstream so upstream_processing is measurable.
		// Not a synchronization primitive — a deliberate latency so
		// the timing partition has non-zero named segments at
		// integer-millisecond resolution.
		time.Sleep(upstreamDelay)
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
	respRow := rows[1]

	timing, ok := respRow["timing_ms"].(map[string]any)
	if !ok {
		t.Fatalf("timing_ms missing from resp row: %+v", respRow)
	}
	stream, ok := respRow["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream missing from resp row: %+v", respRow)
	}

	// All eight fields present.
	for _, key := range []string{
		"client_to_mesh", "mesh_translation_in", "mesh_to_upstream",
		"upstream_processing", "mesh_translation_out", "mesh_to_client",
		"other", "total",
	} {
		if _, present := timing[key]; !present {
			t.Errorf("timing_ms.%s missing", key)
		}
	}

	// D1: timing_ms.total == stream.total_ms exactly.
	timingTotal, _ := timing["total"].(float64)
	streamTotal, _ := stream["total_ms"].(float64)
	if timingTotal != streamTotal {
		t.Errorf("timing_ms.total=%v != stream.total_ms=%v", timingTotal, streamTotal)
	}

	// Partition closes: six named + other == total.
	sum := 0.0
	for _, key := range []string{
		"client_to_mesh", "mesh_translation_in", "mesh_to_upstream",
		"upstream_processing", "mesh_translation_out", "mesh_to_client",
		"other",
	} {
		v, _ := timing[key].(float64)
		if v < 0 {
			t.Errorf("timing_ms.%s=%v, must be non-negative", key, v)
		}
		sum += v
	}
	if sum != timingTotal {
		t.Errorf("partition broken: sum(named+other)=%v != total=%v; timing=%+v", sum, timingTotal, timing)
	}

	// httptrace-plumbing check. With upstreamDelay >> 1 ms,
	// upstream_processing must be non-zero on the success path. If
	// the httptrace callbacks were removed entirely, the upstream
	// time would all collect into other instead. The test fails
	// loudly in that case rather than silently passing the
	// partition-closure check above.
	mt := func(k string) float64 { v, _ := timing[k].(float64); return v }
	if mt("upstream_processing") <= 0 {
		t.Errorf("upstream_processing=%v with %v upstream delay — httptrace plumbing missing? timing=%+v", mt("upstream_processing"), upstreamDelay, timing)
	}
}

// TestTranslationAudit_TimingMs_ErrorBeforeUpstreamDispatch is the
// §6 third mandatory test from DESIGN_B1_timing.local.md: when the
// request errors out before the upstream HTTP exchange (here: 400 on
// a malformed body), httptrace cannot have fired, so segments 3, 4,
// and 5 must all be zero. Segments 1, 2, 6 plus other still close
// the partition. (Segment 6 is non-zero because the 400 error
// response goes through aw.Write, triggering the firstWriteAt Mark.)
func TestTranslationAudit_TimingMs_ErrorBeforeUpstreamDispatch(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should never be reached — request fails at unmarshal.
		t.Errorf("upstream should not be contacted on a 400 path")
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

	// Malformed JSON — fails inside handleA2O at json.Unmarshal,
	// before any upstream dispatch.
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-4-6","messages":[`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	dir := filepath.Join(logDir, gwName)
	rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 2*time.Second)
	respRow := rows[1]

	timing, ok := respRow["timing_ms"].(map[string]any)
	if !ok {
		t.Fatalf("timing_ms missing on error path: %+v", respRow)
	}
	mt := func(k string) float64 { v, _ := timing[k].(float64); return v }

	// Segments 3, 4, 5 must be zero — httptrace never fired.
	for _, key := range []string{"mesh_to_upstream", "upstream_processing", "mesh_translation_out"} {
		if v := mt(key); v != 0 {
			t.Errorf("timing_ms.%s=%v on error-before-dispatch path, want 0", key, v)
		}
	}

	// Partition still closes.
	sum := mt("client_to_mesh") + mt("mesh_translation_in") + mt("mesh_to_upstream") +
		mt("upstream_processing") + mt("mesh_translation_out") + mt("mesh_to_client") + mt("other")
	if sum != mt("total") {
		t.Errorf("partition broken on error path: sum=%v total=%v timing=%+v", sum, mt("total"), timing)
	}
}
