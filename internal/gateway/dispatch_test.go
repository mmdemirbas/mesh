package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func silentDispatchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newDispatchUpstream(t *testing.T, target string, api string, keyEnvs []string, keyValues []string, policy string) *ResolvedUpstream {
	t.Helper()
	pool, err := NewKeyPool(keyEnvs, keyValues, policy)
	if err != nil {
		t.Fatalf("NewKeyPool: %v", err)
	}
	apiKey := ""
	if len(keyValues) > 0 {
		apiKey = keyValues[0]
	}
	return &ResolvedUpstream{
		Cfg:    UpstreamCfg{Name: "test", Target: target, API: api, Health: HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 2}}},
		Client: &http.Client{Timeout: 2 * time.Second},
		APIKey: apiKey,
		Keys:   pool,
	}
}

// TestDispatch_SingleKeyHappyPath asserts the legacy single-key
// path: one attempt, success, attempt list has one entry.
func TestDispatch_SingleKeyHappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, []string{"E"}, []string{"secret"}, "")
	res := dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(res.Attempts))
	}
	if res.Attempts[0].Outcome != AttemptOK {
		t.Errorf("outcome = %v, want ok", res.Attempts[0].Outcome)
	}
}

// TestDispatch_AnthropicAuthHeader asserts dispatch uses x-api-key
// for anthropic upstreams instead of Authorization: Bearer.
func TestDispatch_AnthropicAuthHeader(t *testing.T) {
	t.Parallel()
	var seenAuth, seenAPIKey, seenVer atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		seenAPIKey.Store(r.Header.Get("x-api-key"))
		seenVer.Store(r.Header.Get("anthropic-version"))
		w.WriteHeader(200)
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIAnthropic, []string{"E"}, []string{"my-key"}, "")
	dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if v := seenAPIKey.Load(); v == nil || v.(string) != "my-key" {
		t.Errorf("x-api-key = %v, want my-key", v)
	}
	if v := seenVer.Load(); v == nil || v.(string) != "2023-06-01" {
		t.Errorf("anthropic-version = %v, want 2023-06-01", v)
	}
	if v := seenAuth.Load(); v != nil && v.(string) != "" {
		t.Errorf("Authorization should be empty for anthropic, got %v", v)
	}
}

// TestDispatch_429RotatesKeys: first call returns 429, second key
// returns 200. dispatch retries within the same upstream and
// returns the success.
func TestDispatch_429RotatesKeys(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, []string{"A", "B"}, []string{"a", "b"}, "round_robin")
	res := dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 200 {
		t.Errorf("final status = %d, want 200", res.StatusCode)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}
	if res.Attempts[0].Outcome != AttemptRateLimited {
		t.Errorf("first outcome = %v, want rate_limited", res.Attempts[0].Outcome)
	}
	if res.Attempts[0].FallbackReason != "rate_limited_rotate_key" {
		t.Errorf("first fallback_reason = %q, want rotate_key", res.Attempts[0].FallbackReason)
	}
	if res.Attempts[1].Outcome != AttemptOK {
		t.Errorf("second outcome = %v, want ok", res.Attempts[1].Outcome)
	}
	// First key should be marked degraded (Retry-After=5).
	first := up.Keys.Keys[0]
	if first.IsUsable(time.Now()) {
		t.Errorf("first key should be degraded after 429")
	}
}

// TestDispatch_AllKeys429: every key returns 429. dispatch tries
// all keys, returns the last 429 with fallback_reason set.
func TestDispatch_AllKeys429(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate"}`))
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, []string{"A", "B", "C"}, []string{"a", "b", "c"}, "round_robin")
	res := dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 429 {
		t.Errorf("final status = %d, want 429", res.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (one per key)", calls.Load())
	}
	if len(res.Attempts) != 3 {
		t.Errorf("attempts = %d, want 3", len(res.Attempts))
	}
	last := res.Attempts[len(res.Attempts)-1]
	if last.FallbackReason != "all_keys_rate_limited" {
		t.Errorf("last fallback_reason = %q, want all_keys_rate_limited", last.FallbackReason)
	}
}

// TestDispatch_5xxNoRetryWithinUpstream: 5xx is not retried within
// the same upstream (chain advance is A.5's job). Single attempt,
// 5xx surfaces.
func TestDispatch_5xxNoRetryWithinUpstream(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, []string{"A", "B"}, []string{"a", "b"}, "round_robin")
	res := dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 503 {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 5xx within upstream)", calls.Load())
	}
	if res.Attempts[0].Outcome != AttemptUpstreamError {
		t.Errorf("outcome = %v", res.Attempts[0].Outcome)
	}
}

// TestDispatch_PassthroughZeroKeys: no keys (passthrough). Single
// attempt, no Authorization header set by dispatch.
func TestDispatch_PassthroughZeroKeys(t *testing.T) {
	t.Parallel()
	var seenAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(200)
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, nil, nil, "")
	res := dispatchWithKeyRotation(context.Background(), up, []byte(`{}`), dispatchOpts{
		ExtraHeaders: map[string]string{"Authorization": "ClientToken xyz"},
	}, silentDispatchLogger())
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(res.Attempts))
	}
	// Caller-supplied auth header passes through verbatim.
	if v := seenAuth.Load(); v == nil || !strings.HasPrefix(v.(string), "ClientToken") {
		t.Errorf("passthrough auth not preserved: %v", v)
	}
}

// TestDispatch_NilUpstreamReturnsError defensive: nil up returns an
// error rather than panicking.
func TestDispatch_NilUpstreamReturnsError(t *testing.T) {
	t.Parallel()
	res := dispatchWithKeyRotation(context.Background(), nil, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.Err == nil {
		t.Errorf("expected error for nil upstream")
	}
}

// TestApplyAuthHeaders covers each branch.
func TestApplyAuthHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		api       string
		wantAuth  string
		wantAPIKey string
	}{
		{APIOpenAI, "Bearer k", ""},
		{APIAnthropic, "", "k"},
		{"unknown", "Bearer k", ""},
	}
	for _, c := range cases {
		h := map[string]string{}
		applyAuthHeaders(h, c.api, "k")
		if h["Authorization"] != c.wantAuth {
			t.Errorf("api=%s Authorization = %q, want %q", c.api, h["Authorization"], c.wantAuth)
		}
		if h["x-api-key"] != c.wantAPIKey {
			t.Errorf("api=%s x-api-key = %q, want %q", c.api, h["x-api-key"], c.wantAPIKey)
		}
	}
}
