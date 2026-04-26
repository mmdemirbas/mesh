package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func pickListenerAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitForListenerReady(t *testing.T, bind string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + bind + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway at %s did not start in time", bind)
}

// TestStreamingPicksFromKeyPool is the post-batch-review regression
// for finding #1: streaming handlers used to read upstream.APIKey
// directly and ignore the multi-key pool entirely. After the fix
// they should call upstream.Keys.Pick() and apply the right
// API-shape auth header.
//
// We exercise both a2o and o2a stream paths via real httptest
// upstreams, and assert that with two keys configured we see both
// keys come through in the Authorization headers under
// round_robin.
func TestStreamingPicksFromKeyPool_A2O(t *testing.T) {
	t.Setenv("STREAM_TEST_KEY_A", "key-aaa")
	t.Setenv("STREAM_TEST_KEY_B", "key-bbb")

	var hits sync.Map // bearer string -> count
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if v, ok := hits.LoadOrStore(auth, 1); ok {
			hits.Store(auth, v.(int)+1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := GatewayCfg{
		Name:   "stream-rot-a2o",
		Client: []ClientCfg{{Bind: pickListenerAddr(t), API: APIAnthropic}},
		Upstream: []UpstreamCfg{{
			Name:           "panshi",
			Target:         upstreamSrv.URL,
			API:            APIOpenAI,
			APIKeyEnvs:     []string{"STREAM_TEST_KEY_A", "STREAM_TEST_KEY_B"},
			RotationPolicy: "round_robin",
		}},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "panshi"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Start(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForListenerReady(t, cfg.Client[0].Bind)

	// Fire two streaming requests; round-robin should send one to
	// each key.
	for i := 0; i < 2; i++ {
		body := `{"model":"claude","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post("http://"+cfg.Client[0].Bind+"/v1/messages", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("post[%d]: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	// Both keys should have been used at least once.
	a, _ := hits.Load("Bearer key-aaa")
	b, _ := hits.Load("Bearer key-bbb")
	if a == nil || b == nil {
		t.Errorf("expected both keys to receive traffic; key-aaa=%v key-bbb=%v", a, b)
	}
}

// TestStreamingPicksFromKeyPool_O2A asserts the o2a-stream path
// picks from the pool and uses x-api-key (Anthropic shape) instead
// of the legacy hardcoded read of upstream.APIKey.
func TestStreamingPicksFromKeyPool_O2A(t *testing.T) {
	t.Setenv("STREAM_TEST_ANTH_KEY_A", "anth-aaa")
	t.Setenv("STREAM_TEST_ANTH_KEY_B", "anth-bbb")

	var hits sync.Map
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if v, ok := hits.LoadOrStore(key, 1); ok {
			hits.Store(key, v.(int)+1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		f.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := GatewayCfg{
		Name:   "stream-rot-o2a",
		Client: []ClientCfg{{Bind: pickListenerAddr(t), API: APIOpenAI}},
		Upstream: []UpstreamCfg{{
			Name:           "anth-multi",
			Target:         upstreamSrv.URL,
			API:            APIAnthropic,
			APIKeyEnvs:     []string{"STREAM_TEST_ANTH_KEY_A", "STREAM_TEST_ANTH_KEY_B"},
			RotationPolicy: "round_robin",
		}},
		Routing: []RoutingRule{{ClientModel: []string{"*"}, UpstreamName: "anth-multi"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Start(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForListenerReady(t, cfg.Client[0].Bind)

	for i := 0; i < 2; i++ {
		body := `{"model":"gpt","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post("http://"+cfg.Client[0].Bind+"/v1/chat/completions", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("post[%d]: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	a, _ := hits.Load("anth-aaa")
	b, _ := hits.Load("anth-bbb")
	if a == nil || b == nil {
		t.Errorf("expected both keys to receive traffic; anth-aaa=%v anth-bbb=%v", a, b)
	}
}
