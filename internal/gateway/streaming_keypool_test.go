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

// TestRecordStreamAttempt_DegradesKeyOn429 is a unit-level regression
// for REVIEW B1: streaming used to skip recordPassiveOutcome entirely.
// The helper now records the per-key state transition the same way
// dispatchWithKeyRotation does. This test pins:
//   - 429 → MarkDegraded with Retry-After window
//   - consec failures incremented on AttemptRateLimited
//   - audit Attempt entry appended with the right fields
func TestRecordStreamAttempt_DegradesKeyOn429(t *testing.T) {
	t.Parallel()
	key := NewKeyState("E", "secret")
	au := &AuditUpstream{}
	up := &ResolvedUpstream{
		Cfg: UpstreamCfg{
			Name:   "panshi",
			API:    APIOpenAI,
			Health: HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 1, Backoff: "30s"}},
		},
	}
	respHeaders := http.Header{"Retry-After": []string{"60"}}

	recordStreamAttempt(au, up, key, time.Now().Add(-50*time.Millisecond), 429, respHeaders, nil, nil)

	if key.IsUsable(time.Now()) {
		t.Errorf("key should be degraded after 429")
	}
	// Retry-After: 60 → degradedUntil should be ~60s from now.
	if !key.IsUsable(time.Now().Add(61 * time.Second)) {
		t.Errorf("key should recover after Retry-After window")
	}
	if len(au.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(au.Attempts))
	}
	att := au.Attempts[0]
	if att.UpstreamName != "panshi" {
		t.Errorf("upstream_name = %q, want panshi", att.UpstreamName)
	}
	if att.KeyID != key.ID {
		t.Errorf("key_id = %q, want %q", att.KeyID, key.ID)
	}
	if att.Outcome != AttemptRateLimited {
		t.Errorf("outcome = %q, want rate_limited", att.Outcome)
	}
	if att.StatusCode != 429 {
		t.Errorf("status = %d, want 429", att.StatusCode)
	}
}

// TestRecordStreamAttempt_NetworkErrorIncrementsConsec pins the
// AttemptNetworkError path: scanner failed mid-stream OR transport
// error before the response. consec_failures bumps; threshold-hit
// degrades.
func TestRecordStreamAttempt_NetworkErrorIncrementsConsec(t *testing.T) {
	t.Parallel()
	key := NewKeyState("E", "secret")
	au := &AuditUpstream{}
	up := &ResolvedUpstream{
		Cfg: UpstreamCfg{
			Name:   "panshi",
			API:    APIOpenAI,
			Health: HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 2, Backoff: "30s"}},
		},
	}

	// One failure: not yet degraded.
	recordStreamAttempt(au, up, key, time.Now(), 0, nil, nil, io.EOF)
	if !key.IsUsable(time.Now()) {
		t.Errorf("one failure should not degrade (threshold=2)")
	}
	// Second failure: hits threshold.
	recordStreamAttempt(au, up, key, time.Now(), 0, nil, nil, io.EOF)
	if key.IsUsable(time.Now()) {
		t.Errorf("second failure should degrade key (threshold=2)")
	}
	if len(au.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(au.Attempts))
	}
	if au.Attempts[0].Outcome != AttemptNetworkError {
		t.Errorf("outcome = %q, want network_error", au.Attempts[0].Outcome)
	}
}

// TestRecordStreamAttempt_OkResetsConsec pins that AttemptOK clears
// consec_failures and lets a previously-failing key recover.
func TestRecordStreamAttempt_OkResetsConsec(t *testing.T) {
	t.Parallel()
	key := NewKeyState("E", "secret")
	au := &AuditUpstream{}
	up := &ResolvedUpstream{
		Cfg: UpstreamCfg{
			Name:   "panshi",
			API:    APIOpenAI,
			Health: HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 2, Backoff: "30s"}},
		},
	}
	recordStreamAttempt(au, up, key, time.Now(), 0, nil, nil, io.EOF)
	if key.Snapshot().ConsecFailures != 1 {
		t.Errorf("consec = %d, want 1", key.Snapshot().ConsecFailures)
	}
	recordStreamAttempt(au, up, key, time.Now(), 200, nil, nil, nil)
	if key.Snapshot().ConsecFailures != 0 {
		t.Errorf("AttemptOK should reset consec; got %d", key.Snapshot().ConsecFailures)
	}
	if au.Attempts[1].Outcome != AttemptOK {
		t.Errorf("outcome = %q, want ok", au.Attempts[1].Outcome)
	}
}

// TestRecordStreamAttempt_NilKeyAndAuditSafe pins the no-op branches:
// passthrough upstreams pick nil keys, and audit-disabled requests
// have nil au. Helper must not panic.
func TestRecordStreamAttempt_NilKeyAndAuditSafe(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic: %v", r)
		}
	}()
	up := &ResolvedUpstream{Cfg: UpstreamCfg{Name: "x", API: APIOpenAI}}
	recordStreamAttempt(nil, up, nil, time.Now(), 200, nil, nil, nil)
	au := &AuditUpstream{}
	recordStreamAttempt(au, up, nil, time.Now(), 200, nil, nil, nil)
	// Even with nil key, the audit attempt still records the upstream.
	if len(au.Attempts) != 1 || au.Attempts[0].UpstreamName != "x" {
		t.Errorf("nil-key audit attempt missing upstream_name: %+v", au.Attempts)
	}
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
