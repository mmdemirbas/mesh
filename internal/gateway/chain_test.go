package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestChain_AdvancesOn5xx asserts: first chain step returns 503,
// dispatch advances to second step which returns 200.
func TestChain_AdvancesOn5xx(t *testing.T) {
	t.Parallel()
	var aHits, bHits atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aHits.Add(1)
		w.WriteHeader(503)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bHits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srvB.Close()
	upA := newDispatchUpstream(t, srvA.URL, APIOpenAI, []string{"A"}, []string{"a"}, "")
	upA.Cfg.Name = "primary"
	upB := newDispatchUpstream(t, srvB.URL, APIOpenAI, []string{"B"}, []string{"b"}, "")
	upB.Cfg.Name = "fallback"

	res := dispatchAcrossChain(context.Background(), []*ResolvedUpstream{upA, upB}, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Errorf("hits a=%d b=%d, want 1/1", aHits.Load(), bHits.Load())
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(res.Attempts))
	}
	if res.Attempts[0].UpstreamName != "primary" {
		t.Errorf("first attempt upstream = %q, want primary", res.Attempts[0].UpstreamName)
	}
	if res.Attempts[1].UpstreamName != "fallback" {
		t.Errorf("second attempt upstream = %q, want fallback", res.Attempts[1].UpstreamName)
	}
	if !strings.Contains(res.Attempts[0].FallbackReason, "chain_advance") {
		t.Errorf("first fallback_reason = %q, want chain_advance...", res.Attempts[0].FallbackReason)
	}
}

// TestChain_4xxNoAdvance: client error doesn't trigger chain
// advance — request shape problem, trying another upstream won't
// fix it.
func TestChain_4xxNoAdvance(t *testing.T) {
	t.Parallel()
	var aHits, bHits atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aHits.Add(1)
		w.WriteHeader(400)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bHits.Add(1)
		w.WriteHeader(200)
	}))
	defer srvB.Close()
	upA := newDispatchUpstream(t, srvA.URL, APIOpenAI, []string{"A"}, []string{"a"}, "")
	upB := newDispatchUpstream(t, srvB.URL, APIOpenAI, []string{"B"}, []string{"b"}, "")
	res := dispatchAcrossChain(context.Background(), []*ResolvedUpstream{upA, upB}, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (4xx surfaces, no advance)", res.StatusCode)
	}
	if bHits.Load() != 0 {
		t.Errorf("fallback hit on 4xx, should not advance")
	}
}

// TestChain_429AdvancesAfterAllKeysExhausted: chain step's keys
// all 429, dispatch advances to next step.
func TestChain_429AdvancesAfterAllKeysExhausted(t *testing.T) {
	t.Parallel()
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer srvA.Close()
	var bHits atomic.Int32
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bHits.Add(1)
		w.WriteHeader(200)
	}))
	defer srvB.Close()
	upA := newDispatchUpstream(t, srvA.URL, APIOpenAI, []string{"A1", "A2"}, []string{"a1", "a2"}, "round_robin")
	upB := newDispatchUpstream(t, srvB.URL, APIOpenAI, []string{"B"}, []string{"b"}, "")
	res := dispatchAcrossChain(context.Background(), []*ResolvedUpstream{upA, upB}, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 200 {
		t.Errorf("final status = %d, want 200", res.StatusCode)
	}
	if bHits.Load() != 1 {
		t.Errorf("fallback should be hit once after primary's keys exhausted")
	}
	// Attempts should include 2 from upA + 1 from upB.
	if len(res.Attempts) != 3 {
		t.Errorf("attempts = %d, want 3", len(res.Attempts))
	}
}

// TestChain_AllExhaustedReturnsLastError
func TestChain_AllExhaustedReturnsLastError(t *testing.T) {
	t.Parallel()
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv503.Close()
	upA := newDispatchUpstream(t, srv503.URL, APIOpenAI, []string{"A"}, []string{"a"}, "")
	upB := newDispatchUpstream(t, srv503.URL, APIOpenAI, []string{"B"}, []string{"b"}, "")
	res := dispatchAcrossChain(context.Background(), []*ResolvedUpstream{upA, upB}, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 503 {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}
}

// TestChain_SingleElementBehaviorIdenticalToDispatch: a one-
// element chain is equivalent to dispatchWithKeyRotation directly.
func TestChain_SingleElementBehaviorIdenticalToDispatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	up := newDispatchUpstream(t, srv.URL, APIOpenAI, []string{"E"}, []string{"v"}, "")
	res := dispatchAcrossChain(context.Background(), []*ResolvedUpstream{up}, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(res.Attempts))
	}
}

// TestChain_EmptyReturnsError: zero-length chain returns the
// nil-upstream error rather than panicking.
func TestChain_EmptyReturnsError(t *testing.T) {
	t.Parallel()
	res := dispatchAcrossChain(context.Background(), nil, []byte(`{}`), dispatchOpts{}, silentDispatchLogger())
	if res.Err == nil {
		t.Errorf("expected error for empty chain")
	}
}

// TestRouteChain_ResolvesToList
func TestRouteChain_ResolvesToList(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name:   "chain",
		Client: []ClientCfg{{Bind: "127.0.0.1:3457", API: APIOpenAI}},
		Upstream: []UpstreamCfg{
			{Name: "primary", Target: "http://p.example/v1", API: APIOpenAI},
			{Name: "secondary", Target: "http://s.example/v1", API: APIOpenAI},
		},
		Routing: []RoutingRule{{
			ClientModel:   []string{"*"},
			UpstreamChain: []string{"primary", "secondary"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r, err := NewRouter(cfg, silentDispatchLogger())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	chain := r.RouteChain("anything")
	if len(chain) != 2 {
		t.Fatalf("chain len = %d, want 2", len(chain))
	}
	if chain[0].Cfg.Name != "primary" || chain[1].Cfg.Name != "secondary" {
		t.Errorf("chain order: %v / %v", chain[0].Cfg.Name, chain[1].Cfg.Name)
	}
	// Route() returns first chain element.
	if r.Route("any").Cfg.Name != "primary" {
		t.Errorf("Route() should return first chain element")
	}
}

// TestValidate_ChainRejectsMixedAPI
func TestValidate_ChainRejectsMixedAPI(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name:   "x",
		Client: []ClientCfg{{Bind: "127.0.0.1:0", API: APIAnthropic}},
		Upstream: []UpstreamCfg{
			{Name: "anth", Target: "http://a.example/v1", API: APIAnthropic},
			{Name: "oai", Target: "http://o.example/v1", API: APIOpenAI},
		},
		Routing: []RoutingRule{{
			ClientModel:   []string{"*"},
			UpstreamChain: []string{"anth", "oai"},
		}},
	}
	// Use any free port (0) to avoid bind validation; we only care
	// about chain validation here.
	cfg.Client[0].Bind = "127.0.0.1:0"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "homogeneous APIs") {
		t.Errorf("expected mixed-API rejection, got %v", err)
	}
}

// TestValidate_ChainRejectsDuplicates
func TestValidate_ChainRejectsDuplicates(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name:   "x",
		Client: []ClientCfg{{Bind: "127.0.0.1:3457", API: APIOpenAI}},
		Upstream: []UpstreamCfg{
			{Name: "a", Target: "http://a.example/v1", API: APIOpenAI},
		},
		Routing: []RoutingRule{{
			ClientModel:   []string{"*"},
			UpstreamChain: []string{"a", "a"},
		}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("expected duplicate rejection, got %v", err)
	}
}

// TestValidate_ChainRejectsBothForms
func TestValidate_ChainRejectsBothForms(t *testing.T) {
	t.Parallel()
	cfg := &GatewayCfg{
		Name:   "x",
		Client: []ClientCfg{{Bind: "127.0.0.1:3457", API: APIOpenAI}},
		Upstream: []UpstreamCfg{
			{Name: "a", Target: "http://a.example/v1", API: APIOpenAI},
		},
		Routing: []RoutingRule{{
			ClientModel:   []string{"*"},
			UpstreamName:  "a",
			UpstreamChain: []string{"a"},
		}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive rejection, got %v", err)
	}
}

// TestShouldAdvanceChain unit tests the per-outcome decision.
func TestShouldAdvanceChain(t *testing.T) {
	t.Parallel()
	cases := map[AttemptOutcome]bool{
		AttemptOK:            false,
		AttemptClientError:   false,
		AttemptRateLimited:   true,
		AttemptUpstreamError: true,
		AttemptNetworkError:  true,
		AttemptTimeout:       true,
	}
	for o, want := range cases {
		if got := shouldAdvanceChain(o); got != want {
			t.Errorf("shouldAdvanceChain(%v) = %v, want %v", o, got, want)
		}
	}
}

// pin time import for tests above that don't directly touch it but
// share the package's test scaffolding.
var _ = time.Second
