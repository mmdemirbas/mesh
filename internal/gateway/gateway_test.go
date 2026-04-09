package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGateway_A2O_NonStreaming(t *testing.T) {
	t.Parallel()
	// Mock OpenAI upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}

		body, _ := io.ReadAll(r.Body)
		var req ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("cannot parse upstream request: %v", err)
			w.WriteHeader(500)
			return
		}
		if req.Model != "glm-4.7" {
			t.Errorf("upstream model = %q, want glm-4.7", req.Model)
		}

		resp := ChatCompletionResponse{
			ID:      "chatcmpl-test",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "glm-4.7",
			Choices: []OpenAIChoice{
				{
					Index: 0,
					Message: OpenAIMsg{
						Role:    "assistant",
						Content: json.RawMessage(`"Hello from GLM"`),
					},
					FinishReason: "stop",
				},
			},
			Usage: &OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-a2o",
		Bind:     "127.0.0.1:0", // will use listener
		Mode:     ModeAnthropicToOpenAI,
		Upstream: upstream.URL,
		ModelMap: map[string]string{"claude-sonnet-4-6": "glm-4.7"},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start gateway on random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close() // gateway.Start will re-listen

	errCh := make(chan error, 1)
	go func() {
		errCh <- Start(ctx, cfg, slog.Default())
	}()

	// Wait for gateway to start.
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	// Send Anthropic request.
	reqBody := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var anthResp MessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthResp); err != nil {
		t.Fatal(err)
	}

	if anthResp.Type != "message" {
		t.Errorf("type = %q, want message", anthResp.Type)
	}
	if anthResp.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6 (original client model)", anthResp.Model)
	}
	if anthResp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", anthResp.StopReason)
	}
	if len(anthResp.Content) < 1 || anthResp.Content[0].Text != "Hello from GLM" {
		t.Errorf("content = %v", anthResp.Content)
	}

	cancel()
}

func TestGateway_O2A_NonStreaming(t *testing.T) {
	// Mock Anthropic upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing or wrong x-api-key")
		}

		body, _ := io.ReadAll(r.Body)
		var req MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("cannot parse upstream request: %v", err)
			w.WriteHeader(500)
			return
		}
		if req.Model != "claude-sonnet-4-6" {
			t.Errorf("upstream model = %q, want claude-sonnet-4-6", req.Model)
		}

		resp := MessagesResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Model:      "claude-sonnet-4-6",
			StopReason: "end_turn",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello from Claude"},
			},
			Usage: AnthropicUsage{InputTokens: 10, OutputTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer upstream.Close()

	t.Setenv("TEST_ANTH_KEY", "test-key")

	cfg := GatewayCfg{
		Name:      "test-o2a",
		Bind:      "127.0.0.1:0",
		Mode:      ModeOpenAIToAnthropic,
		Upstream:  upstream.URL,
		APIKeyEnv: "TEST_ANTH_KEY",
		ModelMap:  map[string]string{"gpt-4o": "claude-sonnet-4-6"},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{
		"model": "gpt-4o",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var oaiResp ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		t.Fatal(err)
	}

	if oaiResp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", oaiResp.Object)
	}
	if oaiResp.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (original client model)", oaiResp.Model)
	}
	if len(oaiResp.Choices) < 1 {
		t.Fatal("no choices")
	}
	if oaiResp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", oaiResp.Choices[0].FinishReason)
	}
	var text string
	json.Unmarshal(oaiResp.Choices[0].Message.Content, &text)
	if text != "Hello from Claude" {
		t.Errorf("content = %q, want 'Hello from Claude'", text)
	}

	cancel()
}

func TestGateway_Health(t *testing.T) {
	t.Parallel()
	cfg := GatewayCfg{
		Name:     "test-health",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: "http://localhost:9999", // not used
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	resp, err := http.Get("http://" + cfg.Bind + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	cancel()
}

func TestGateway_UpstreamError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-error",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: upstream.URL,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Direction A: upstream 429 -> client 429.
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}

	cancel()
}

func TestGateway_O2A_UpstreamError_529(t *testing.T) {
	t.Parallel()
	// Anthropic 529 (overloaded) -> OpenAI client sees 503.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-529",
		Bind:     "127.0.0.1:0",
		Mode:     ModeOpenAIToAnthropic,
		Upstream: upstream.URL,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503 (Anthropic 529 -> OpenAI 503)", resp.StatusCode)
	}

	cancel()
}

func TestGateway_UpstreamError_ResponseFormat(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`rate limited`)) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-err-format",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: upstream.URL,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	// Verify the error body is valid Anthropic error JSON.
	var errResp AnthropicErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("cannot decode error response: %v", err)
	}
	if errResp.Type != "error" {
		t.Errorf("type = %q, want error", errResp.Type)
	}
	if errResp.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", errResp.Error.Type)
	}

	cancel()
}

func TestGateway_A2O_InvalidJSON(t *testing.T) {
	t.Parallel()
	cfg := GatewayCfg{
		Name:     "test-bad-json",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: "http://localhost:9999",
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() { Start(ctx, cfg, slog.Default()) }() //nolint:errcheck
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	cancel()
}

func TestGateway_O2A_InvalidJSON(t *testing.T) {
	t.Parallel()
	cfg := GatewayCfg{
		Name:     "test-bad-json-o2a",
		Bind:     "127.0.0.1:0",
		Mode:     ModeOpenAIToAnthropic,
		Upstream: "http://localhost:9999",
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() { Start(ctx, cfg, slog.Default()) }() //nolint:errcheck
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	resp, err := http.Post("http://"+cfg.Bind+"/v1/chat/completions", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	cancel()
}

func TestGateway_A2O_UpstreamConnectionFailure(t *testing.T) {
	t.Parallel()
	// Upstream on a port that refuses connections.
	cfg := GatewayCfg{
		Name:     "test-conn-fail",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: "http://127.0.0.1:1", // port 1 — connection refused
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() { Start(ctx, cfg, slog.Default()) }() //nolint:errcheck
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	cancel()
}

func TestGateway_A2O_StreamUpstreamError(t *testing.T) {
	t.Parallel()
	// Upstream returns 500 for a streaming request.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-stream-err",
		Bind:     "127.0.0.1:0",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: upstream.URL,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() { Start(ctx, cfg, slog.Default()) }() //nolint:errcheck
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	cancel()
}

func TestGateway_O2A_StreamUpstreamError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"too fast"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	cfg := GatewayCfg{
		Name:     "test-stream-err-o2a",
		Bind:     "127.0.0.1:0",
		Mode:     ModeOpenAIToAnthropic,
		Upstream: upstream.URL,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() { Start(ctx, cfg, slog.Default()) }() //nolint:errcheck
	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := `{"model":"gpt-4o","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post("http://"+cfg.Bind+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}

	cancel()
}

// waitForHTTP polls a URL until it returns 200 or the timeout expires.
func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatalf("timeout waiting for %s", url)
			return
		case <-ticker.C:
			resp, err := http.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
		}
	}
}
