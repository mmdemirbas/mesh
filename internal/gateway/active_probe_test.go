package gateway

import (
	"bytes"
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

func silentProbeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestActiveProbe_PicksRandomUsableKey(t *testing.T) {
	t.Parallel()
	pool, _ := NewKeyPool([]string{"A", "B", "C"}, []string{"a", "b", "c"}, "round_robin")
	now := time.Now()
	// Mark A degraded; should never be picked.
	pool.Keys[0].MarkDegraded(now.Add(time.Hour))
	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		k := pickProbeKey(pool, now)
		if k == nil {
			t.Fatal("nil pick with 2 usable keys")
		}
		if k.ID == pool.Keys[0].ID {
			t.Errorf("picked degraded key A on iter %d", i)
		}
		seen[k.ID]++
	}
	// Both B and C should appear over 50 picks.
	if len(seen) != 2 {
		t.Errorf("probe key picks not random across both usable keys: %v", seen)
	}
}

func TestActiveProbe_AllDegradedReturnsNil(t *testing.T) {
	t.Parallel()
	pool, _ := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "round_robin")
	now := time.Now()
	for _, k := range pool.Keys {
		k.MarkDegraded(now.Add(time.Hour))
	}
	if got := pickProbeKey(pool, now); got != nil {
		t.Errorf("all-degraded pool should return nil, got %+v", got)
	}
}

func TestActiveProbe_NilSafe(t *testing.T) {
	t.Parallel()
	if got := pickProbeKey(nil, time.Now()); got != nil {
		t.Errorf("nil pool: got %+v, want nil", got)
	}
}

func TestActiveInterval_DefaultAndOverride(t *testing.T) {
	t.Parallel()
	if activeInterval(ActiveHealthCfg{}) != defaultActiveProbeInterval {
		t.Errorf("default interval not honored")
	}
	if activeInterval(ActiveHealthCfg{Interval: "5s"}) != 5*time.Second {
		t.Errorf("override 5s not honored")
	}
	if activeInterval(ActiveHealthCfg{Interval: "garbage"}) != defaultActiveProbeInterval {
		t.Errorf("bad interval should fall back to default")
	}
}

func TestActiveTimeout_DefaultAndOverride(t *testing.T) {
	t.Parallel()
	if activeTimeout(ActiveHealthCfg{}) != defaultActiveProbeTimeout {
		t.Errorf("default timeout not honored")
	}
	if activeTimeout(ActiveHealthCfg{Timeout: "3s"}) != 3*time.Second {
		t.Errorf("override 3s not honored")
	}
}

// TestRunOneProbe_RecordsOutcome stands up a tiny in-process upstream
// that returns 500 and verifies one probe degrades a single-key
// pool whose threshold is 1.
func TestRunOneProbe_DegradesKeyOnUpstreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	pool, _ := NewKeyPool([]string{"E"}, []string{"v"}, "")
	up := &ResolvedUpstream{
		Cfg: UpstreamCfg{
			Name:   "test",
			Target: srv.URL,
			API:    APIOpenAI,
			Health: HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 1}, Active: ActiveHealthCfg{Enabled: true, Payload: "{}"}},
		},
		Client: &http.Client{Timeout: 2 * time.Second},
		Keys:   pool,
	}
	runOneProbe(context.Background(), "test", up, []byte("{}"), 2*time.Second, silentProbeLogger())
	if pool.Keys[0].IsUsable(time.Now()) {
		t.Errorf("key should be degraded after 500 with threshold=1")
	}
}

func TestRunOneProbe_SuccessRecordsHealthy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	pool, _ := NewKeyPool([]string{"E"}, []string{"v"}, "")
	up := &ResolvedUpstream{
		Cfg: UpstreamCfg{
			Name: "test", Target: srv.URL, API: APIOpenAI,
			Health: HealthCfg{Active: ActiveHealthCfg{Enabled: true, Payload: "{}"}},
		},
		Client: &http.Client{Timeout: 2 * time.Second},
		Keys:   pool,
	}
	runOneProbe(context.Background(), "test", up, []byte("{}"), 2*time.Second, silentProbeLogger())
	snap := pool.Keys[0].Snapshot()
	if snap.Successes == 0 {
		t.Errorf("expected success counter increment")
	}
}

func TestRunOneProbe_AuthHeaderSent(t *testing.T) {
	t.Parallel()
	var seen atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.WriteHeader(200)
	}))
	defer srv.Close()
	pool, _ := NewKeyPool([]string{"E"}, []string{"the-secret"}, "")
	up := &ResolvedUpstream{
		Cfg:    UpstreamCfg{Name: "test", Target: srv.URL, API: APIOpenAI, Health: HealthCfg{Active: ActiveHealthCfg{Enabled: true, Payload: "{}"}}},
		Client: &http.Client{Timeout: 2 * time.Second},
		Keys:   pool,
	}
	runOneProbe(context.Background(), "test", up, []byte("{}"), 2*time.Second, silentProbeLogger())
	got := seen.Load()
	if got == nil || got.(string) != "Bearer the-secret" {
		t.Errorf("Authorization header = %v, want 'Bearer the-secret'", got)
	}
}

func TestRunActiveProbes_StartsOnePerEnabledUpstream(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	pool, _ := NewKeyPool([]string{"E"}, []string{"v"}, "")
	router := &Router{
		upstreams: map[string]*ResolvedUpstream{
			"with-probe": {
				Cfg: UpstreamCfg{
					Name: "with-probe", Target: srv.URL, API: APIOpenAI,
					Health: HealthCfg{Active: ActiveHealthCfg{Enabled: true, Interval: "20ms", Payload: "{}"}},
				},
				Client: &http.Client{Timeout: 2 * time.Second},
				Keys:   pool,
			},
			"without-probe": {
				Cfg:    UpstreamCfg{Name: "without-probe", Target: "http://nowhere", API: APIOpenAI},
				Client: &http.Client{Timeout: 2 * time.Second},
				Keys:   pool,
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	runActiveProbes(ctx, router, silentProbeLogger())
	// Wait for a few intervals to fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && hits.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if hits.Load() < 2 {
		t.Errorf("expected ≥2 probe hits, got %d", hits.Load())
	}
}

func TestActivePayloadRequiredWhenEnabled(t *testing.T) {
	t.Parallel()
	hc := HealthCfg{Active: ActiveHealthCfg{Enabled: true}}
	err := hc.validate("test")
	if err == nil {
		t.Errorf("expected error: enabled without payload")
	} else if !strings.Contains(err.Error(), "payload") {
		t.Errorf("error %v should mention payload", err)
	}
	// Setting payload makes it valid.
	hc.Active.Payload = `{"model":"x","messages":[]}`
	if err := hc.validate("test"); err != nil {
		t.Errorf("unexpected error with payload: %v", err)
	}
}

func TestRunActiveProbes_NilRouter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil router panicked: %v", r)
		}
	}()
	runActiveProbes(context.Background(), nil, silentProbeLogger())
}

// pin imports the test binary needs
var _ = bytes.NewBuffer
