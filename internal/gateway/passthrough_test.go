package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
		Name: "gw-" + strings.ReplaceAll(t.Name(), "/", "_"),
		Client: []ClientCfg{
			{Bind: "127.0.0.1:0", API: APIAnthropic},
		},
		Upstream: []UpstreamCfg{
			{Name: "default", Target: upstreamURL, API: APIAnthropic},
		},
		Routing: []RoutingRule{
			{ClientModel: []string{"*"}, UpstreamName: "default"},
		},
		Log: LogCfg{Level: LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	if opts != nil {
		opts(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Start() listens on cfg.Client[0].Bind — use a pre-reserved loopback port to make
	// the URL known ahead of time.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Client[0].Bind = addr

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

	// Wait for the recorder's background goroutine to flush both rows.
	rows := waitForRows(t, func() []map[string]any { return auditFiles(t, gwName, logDir) }, 2, 2*time.Second)
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

// TestPassthrough_ResponseModelOverridesNonStreaming pins
// PLAN_GATEWAY_SEPARATION Part 1 on the buffered passthrough path.
// The client sends "claude-opus-4-6"; the upstream replies with
// "claude-opus-4-6" in the body; the gateway has response_model:
// "internal-claude" so the client should see "internal-claude".
func TestPassthrough_ResponseModelOverridesNonStreaming(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"id":"msg_x","type":"message","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	base := startPassthroughGateway(t, upstream.URL, func(c *GatewayCfg) {
		c.ResponseModel = "internal-claude"
	})

	body := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("parse response: %v; body=%s", err, out)
	}
	if payload["model"] != "internal-claude" {
		t.Errorf("response model = %v, want internal-claude (response_model override should rewrite the buffered body)", payload["model"])
	}
}

// TestRewriteModelInJSON_NestedAnthropicMessage pins the Anthropic
// SSE shape (`{"message":{"model":"..."}}`) for response_model.
func TestRewriteModelInJSON_NestedAnthropicMessage(t *testing.T) {
	t.Parallel()
	in := []byte(`{"type":"message_start","message":{"id":"m","model":"claude-x","usage":{"input_tokens":1}}}`)
	out := rewriteModelInJSON(in, "internal-claude")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := got["message"].(map[string]any)
	if msg["model"] != "internal-claude" {
		t.Errorf("nested message.model = %v, want internal-claude", msg["model"])
	}
}

// TestRewriteModelInJSON_NoModelFieldUnchanged pins that JSON
// without a model field passes through unchanged (e.g.,
// content_block_delta SSE events that don't carry a model).
func TestRewriteModelInJSON_NoModelFieldUnchanged(t *testing.T) {
	t.Parallel()
	in := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	out := rewriteModelInJSON(in, "internal-claude")
	if !bytes.Equal(in, out) {
		t.Errorf("expected unchanged passthrough; got %s", out)
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
		c.Upstream[0].APIKeyEnv = "" // passthrough preserves client auth
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
		c.Upstream[0].APIKeyEnv = envVar
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

	rows := waitForRows(t, func() []map[string]any { return auditFiles(t, gwName, logDir) }, 2, 2*time.Second)
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

	rows := waitForRows(t, func() []map[string]any { return auditFiles(t, gwName, logDir) }, 2, 2*time.Second)
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

func TestDecodeForAudit(t *testing.T) {
	t.Parallel()
	plaintext := `{"hello":"world"}`
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	zl := func(s string) []byte {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	rawFlate := func(s string) []byte {
		var b bytes.Buffer
		w, _ := flate.NewWriter(&b, flate.DefaultCompression)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}

	tests := []struct {
		name     string
		body     []byte
		encoding string
		want     []byte
	}{
		{"identity", []byte(plaintext), "", []byte(plaintext)},
		{"explicit_identity", []byte(plaintext), "identity", []byte(plaintext)},
		{"gzip_decoded", gz(plaintext), "gzip", []byte(plaintext)},
		{"deflate_zlib_decoded", zl(plaintext), "deflate", []byte(plaintext)},
		{"deflate_raw_decoded", rawFlate(plaintext), "deflate", []byte(plaintext)},
		{"unsupported_br_passes_through", []byte("opaque-br-bytes"), "br", []byte("opaque-br-bytes")},
		{"malformed_gzip_passes_through", []byte("not gzip"), "gzip", []byte("not gzip")},
		{"empty_body", nil, "gzip", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := decodeForAudit(tt.body, tt.encoding, logger)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("decodeForAudit(_, %q): got %q want %q", tt.encoding, got, tt.want)
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

// TestParseUsageCacheTokens verifies that prompt-cache and reasoning fields
// surface in the parsed Usage. Without these the per-pair token bar in the UI
// cannot show how much of the input came from cache versus fresh prompt.
func TestParseUsageCacheTokens(t *testing.T) {
	t.Parallel()
	anthropic := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":2000,"cache_read_input_tokens":7000}}`)
	u := parseUsage(anthropic, APIAnthropic)
	if u == nil {
		t.Fatal("anthropic usage parsed as nil")
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 ||
		u.CacheCreationInputTokens != 2000 || u.CacheReadInputTokens != 7000 {
		t.Errorf("anthropic cache tokens not captured: %+v", u)
	}

	openai := []byte(`{"usage":{"prompt_tokens":120,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":25}}}`)
	o := parseUsage(openai, APIOpenAI)
	if o == nil {
		t.Fatal("openai usage parsed as nil")
	}
	if o.InputTokens != 120 || o.OutputTokens != 80 ||
		o.CacheReadInputTokens != 40 || o.ReasoningTokens != 25 {
		t.Errorf("openai detail tokens not captured: %+v", o)
	}

	// Cache-only response (zero in/out) must still surface as non-nil so the
	// audit row records the cache effectiveness.
	cacheOnly := []byte(`{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":500}}`)
	if c := parseUsage(cacheOnly, APIAnthropic); c == nil || c.CacheReadInputTokens != 500 {
		t.Errorf("cache-only anthropic usage = %+v", c)
	}
}
