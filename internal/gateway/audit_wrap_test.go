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
		Name:        gwName,
		Bind:        addr,
		Upstream:    upstreamURL,
		ClientAPI:   clientAPI,
		UpstreamAPI: upstreamAPI,
		Log:         LogCfg{Level: LogLevelFull, Dir: logDir, MaxFileSize: "10MB", MaxAge: "720h"},
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

	time.Sleep(50 * time.Millisecond)
	rows := readRows(t, filepath.Join(logDir, gwName))
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

	time.Sleep(80 * time.Millisecond)
	rows := readRows(t, filepath.Join(logDir, gwName))
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
