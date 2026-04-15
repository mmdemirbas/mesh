package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startPassthroughGateway starts a Start()-backed gateway on a random loopback
// port and returns the base URL. The recorder directory is placed under
// t.TempDir().
func startPassthroughGateway(t *testing.T, upstreamURL string, opts func(*GatewayCfg)) string {
	t.Helper()
	dir := t.TempDir()
	cfg := GatewayCfg{
		Name:        "gw-" + strings.ReplaceAll(t.Name(), "/", "_"),
		Bind:        "127.0.0.1:0",
		Upstream:    upstreamURL,
		ClientAPI:   APIAnthropic,
		UpstreamAPI: APIAnthropic,
		Log:         LogCfg{Level: LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	if opts != nil {
		opts(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Start() listens on cfg.Bind — use a pre-reserved loopback port to make
	// the URL known ahead of time.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Bind = addr

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
	// Wait for /health to succeed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway did not start in time")
	return ""
}

func auditFiles(t *testing.T, gwName string, logDir string) []map[string]any {
	t.Helper()
	dir := filepath.Join(logDir, gwName)
	return readRows(t, dir)
}

func TestPassthrough_NonStreamingRoundtrip(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "claude-opus-4-6" {
			t.Errorf("upstream model = %v", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"id":"msg_abc","type":"message","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":7}}`)
	}))
	defer upstream.Close()

	var logDir, gwName string
	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		logDir = c.Log.Dir
		gwName = c.Name
	})

	body := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("parse response: %v; body=%s", err, out)
	}
	if payload["id"] != "msg_abc" {
		t.Errorf("response id = %v, want msg_abc (upstream body must be forwarded verbatim)", payload["id"])
	}

	// Flush any file buffers by shutting the gateway (Cleanup handles it) and
	// then reading the audit file.
	time.Sleep(50 * time.Millisecond) // give the recorder goroutine time to write
	rows := auditFiles(t, gwName, logDir)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2: %+v", len(rows), rows)
	}
	reqRow, respRow := rows[0], rows[1]
	if reqRow["direction"] != "a2a" {
		t.Errorf("direction = %v, want a2a", reqRow["direction"])
	}
	if reqRow["model"] != "claude-opus-4-6" {
		t.Errorf("audit model = %v", reqRow["model"])
	}
	if respRow["status"].(float64) != 200 {
		t.Errorf("status = %v", respRow["status"])
	}
	usage := respRow["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 7 {
		t.Errorf("parsed usage = %v, want in=12 out=7", usage)
	}
}

func TestPassthrough_PreservesClientAuthWhenAPIKeyEnvUnset(t *testing.T) {
	t.Parallel()
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		c.APIKeyEnv = "" // passthrough preserves client auth
	})

	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("Authorization", "Bearer client-oauth-token-xyz")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	if gotAuth != "Bearer client-oauth-token-xyz" {
		t.Errorf("upstream Authorization = %q, want client's token preserved", gotAuth)
	}
}

func TestPassthrough_OverwritesAuthWhenAPIKeyEnvSet(t *testing.T) {
	// No t.Parallel — t.Setenv cannot coexist with parallel tests.
	const envVar = "TEST_PASSTHROUGH_AUTH_KEY"
	const key = "sk-gateway-configured-key"
	t.Setenv(envVar, key)

	var gotAuth, gotXAPI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPI = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		c.APIKeyEnv = envVar
		c.ClientAPI = APIAnthropic
		c.UpstreamAPI = APIAnthropic
	})

	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("Authorization", "Bearer client-token-should-be-stripped")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	if strings.Contains(gotAuth, "client-token-should-be-stripped") {
		t.Errorf("client Authorization leaked to upstream: %q", gotAuth)
	}
	if gotXAPI != key {
		t.Errorf("upstream X-Api-Key = %q, want %q", gotXAPI, key)
	}
}

func TestPassthrough_StreamingForwardsBytes(t *testing.T) {
	t.Parallel()

	// Upstream emits three SSE events with explicit flushes.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			f.Flush()
		}
	}))
	defer upstream.Close()

	var logDir, gwName string
	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		logDir = c.Log.Dir
		gwName = c.Name
	})

	req, _ := http.NewRequest("POST", base+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-6","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the stream fully.
	full, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"message_start", "hello ", "message_stop"} {
		if !bytes.Contains(full, []byte(want)) {
			t.Errorf("stream missing %q; body=%s", want, full)
		}
	}

	time.Sleep(100 * time.Millisecond)
	rows := auditFiles(t, gwName, logDir)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	respRow := rows[1]
	if respRow["outcome"] != OutcomeOK {
		t.Errorf("outcome = %v, want ok", respRow["outcome"])
	}
	// Streamed response body is tee'd into audit as a single string.
	bodyStr, ok := respRow["body"].(string)
	if !ok {
		t.Fatalf("full-level streaming audit body type = %T", respRow["body"])
	}
	for _, want := range []string{"message_start", "hello ", "message_stop"} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("audit body missing %q: %s", want, bodyStr)
		}
	}
	// stream_summary must reassemble the text deltas into a single field.
	summary, ok := respRow["stream_summary"].(map[string]any)
	if !ok {
		t.Fatalf("stream_summary missing or wrong type: %T", respRow["stream_summary"])
	}
	if summary["content"] != "hello " {
		t.Errorf("stream_summary.content = %v, want %q", summary["content"], "hello ")
	}
}

func TestPassthrough_ClientCancelProducesCancelledOutcome(t *testing.T) {
	t.Parallel()

	// Upstream holds the connection open; client will cancel.
	ready := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
		f.Flush()
		once.Do(func() { close(ready) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	var logDir, gwName string
	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		logDir = c.Log.Dir
		gwName = c.Name
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-6","stream":true}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// Read first chunk to confirm stream started, then cancel mid-stream.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	<-ready
	cancel()
	_ = resp.Body.Close()

	// Wait for the recorder to flush the cancelled row.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows := auditFiles(t, gwName, logDir)
		if len(rows) >= 2 {
			respRow := rows[1]
			if respRow["outcome"] == OutcomeClientCancelled {
				return // success
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows := auditFiles(t, gwName, logDir)
	t.Fatalf("expected client_cancelled outcome in audit; got rows=%+v", rows)
}

func TestPassthrough_GzippedResponseDecodedInAuditOnly(t *testing.T) {
	t.Parallel()

	gzBody := func(s string) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return buf.Bytes()
	}

	plaintext := `{"id":"msg_xyz","content":[{"type":"text","text":"decoded"}],"usage":{"input_tokens":3,"output_tokens":5}}`
	compressed := gzBody(plaintext)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	var logDir, gwName string
	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		logDir = c.Log.Dir
		gwName = c.Name
	})

	// Explicit Accept-Encoding prevents Go's http.Client from silently
	// auto-decompressing (which only kicks in when Go itself added the header).
	// This mirrors Claude Code, which sets Accept-Encoding on its own.
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The wire bytes to the client must remain gzip — passthrough is transparent.
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("client Content-Encoding = %q, want gzip (wire must stay as upstream sent it)", enc)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, compressed) {
		t.Errorf("client bytes differ from upstream (got %d bytes, want %d); passthrough must not mutate payload", len(got), len(compressed))
	}

	time.Sleep(50 * time.Millisecond)
	rows := auditFiles(t, gwName, logDir)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	respRow := rows[1]
	// full level: body is decoded JSON, not a compressed blob.
	body, ok := respRow["body"].(map[string]any)
	if !ok {
		t.Fatalf("audit body type = %T, want parsed JSON (gzip should be decoded)", respRow["body"])
	}
	if body["id"] != "msg_xyz" {
		t.Errorf("audit body id = %v, want msg_xyz", body["id"])
	}
	usage := respRow["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 3 || usage["output_tokens"].(float64) != 5 {
		t.Errorf("usage parsed from decoded body = %v, want in=3 out=5", usage)
	}
}

func TestBuildUpstreamURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		upstream string
		path     string
		query    string
		want     string
	}{
		{"base_url_preserves_client_path", "https://api.anthropic.com", "/v1/messages", "", "https://api.anthropic.com/v1/messages"},
		{"base_url_with_trailing_slash", "https://api.anthropic.com/", "/v1/messages", "", "https://api.anthropic.com/v1/messages"},
		{"fixed_upstream_path_used_as_is", "https://oneapi.example.com/v1/chat/completions", "/v1/messages", "", "https://oneapi.example.com/v1/chat/completions"},
		{"query_forwarded", "https://api.anthropic.com", "/v1/messages", "beta=2024-01", "https://api.anthropic.com/v1/messages?beta=2024-01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqURL, _ := http.NewRequest("GET", "http://placeholder"+tt.path+"?"+tt.query, nil)
			got, err := buildUpstreamURL(tt.upstream, reqURL.URL)
			if err != nil {
				t.Fatalf("buildUpstreamURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("buildUpstreamURL(%q, %q) = %q, want %q", tt.upstream, tt.path, got, tt.want)
			}
		})
	}
}

func TestParseUsage(t *testing.T) {
	t.Parallel()
	anthropicBody := []byte(`{"usage":{"input_tokens":12,"output_tokens":34}}`)
	openaiBody := []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	empty := []byte(`{}`)

	if u := parseUsage(anthropicBody, APIAnthropic); u == nil || u.InputTokens != 12 || u.OutputTokens != 34 {
		t.Errorf("anthropic = %+v", u)
	}
	if u := parseUsage(openaiBody, APIOpenAI); u == nil || u.InputTokens != 5 || u.OutputTokens != 6 {
		t.Errorf("openai = %+v", u)
	}
	if u := parseUsage(empty, APIAnthropic); u != nil {
		t.Errorf("missing usage should return nil, got %+v", u)
	}
	if u := parseUsage([]byte("not json"), APIAnthropic); u != nil {
		t.Errorf("invalid json should return nil, got %+v", u)
	}
}
