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
