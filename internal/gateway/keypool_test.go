package gateway

import (
	"context"
	"testing"
	"time"
)

func TestNewKeyPool_EmptyMeansPassthrough(t *testing.T) {
	t.Parallel()
	p, err := NewKeyPool(nil, nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Len() != 0 {
		t.Errorf("Len = %d, want 0", p.Len())
	}
	if k := p.Pick(context.Background(), RequestContext{Now: time.Now()}); k != nil {
		t.Errorf("Pick on empty pool should be nil, got %+v", k)
	}
}

func TestNewKeyPool_SingleKeyUsesSinglePolicy(t *testing.T) {
	t.Parallel()
	p, err := NewKeyPool([]string{"E"}, []string{"secret"}, "round_robin") // policy ignored
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Policy.Name() != "single" {
		t.Errorf("single-key pool should use 'single' policy, got %q", p.Policy.Name())
	}
}

func TestNewKeyPool_MultiKeyDefaultsToRoundRobin(t *testing.T) {
	t.Parallel()
	p, err := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Policy.Name() != "round_robin" {
		t.Errorf("default policy = %q, want round_robin", p.Policy.Name())
	}
}

func TestNewKeyPool_UnknownPolicyErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "bogus"); err == nil {
		t.Errorf("unknown policy should error")
	}
}

func TestKeyPool_PickRotatesAndSkipsDegraded(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B", "C"}, []string{"a", "b", "c"}, "round_robin")
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	rc := RequestContext{Now: now}

	// Three picks should hit each key once (round_robin).
	picks := []string{
		p.Pick(context.Background(), rc).ID,
		p.Pick(context.Background(), rc).ID,
		p.Pick(context.Background(), rc).ID,
	}
	seen := map[string]int{picks[0]: 1, picks[1]: 1, picks[2]: 1}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct picks, got %v", picks)
	}

	// Degrade one key; subsequent picks should skip it.
	p.Keys[0].MarkDegraded(now.Add(5 * time.Minute))
	for i := 0; i < 6; i++ {
		k := p.Pick(context.Background(), rc)
		if k == p.Keys[0] {
			t.Errorf("picked degraded key on iteration %d", i)
		}
	}
}

func TestKeyPool_PickAllDegradedReturnsNil(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "round_robin")
	now := time.Now()
	for _, k := range p.Keys {
		k.MarkDegraded(now.Add(time.Hour))
	}
	if got := p.Pick(context.Background(), RequestContext{Now: now}); got != nil {
		t.Errorf("expected nil with all keys degraded, got %+v", got)
	}
	if p.AnyUsable(now) {
		t.Errorf("AnyUsable should be false")
	}
}

func TestKeyPool_LRUPicksLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B", "C"}, []string{"a", "b", "c"}, "lru")
	now := time.Now()
	// Mark A and B as recently used; C never used.
	p.Keys[0].MarkUsed(now.Add(-1 * time.Hour))
	p.Keys[1].MarkUsed(now.Add(-30 * time.Minute))
	// LRU should pick C (never used).
	got := p.Pick(context.Background(), RequestContext{Now: now})
	if got != p.Keys[2] {
		t.Errorf("LRU picked %v, want C (never-used)", got.ID)
	}
}

func TestKeyPool_StickySessionReuses(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B", "C"}, []string{"a", "b", "c"}, "sticky_session")
	now := time.Now()
	rc := RequestContext{Now: now, SessionID: "session-1"}
	first := p.Pick(context.Background(), rc)
	if first == nil {
		t.Fatal("first pick nil")
	}
	for i := 0; i < 5; i++ {
		got := p.Pick(context.Background(), rc)
		if got != first {
			t.Errorf("sticky_session picked different key on iter %d: %v != %v", i, got.ID, first.ID)
		}
	}
	// Different session → likely different key (or possibly the
	// same one if there's only the first usable one). Just check it
	// works.
	rc2 := RequestContext{Now: now, SessionID: "session-2"}
	got := p.Pick(context.Background(), rc2)
	if got == nil {
		t.Errorf("session-2 pick was nil")
	}
}

func TestKeyPool_StickySessionFallsThroughOnDegrade(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "sticky_session")
	now := time.Now()
	rc := RequestContext{Now: now, SessionID: "stick"}
	first := p.Pick(context.Background(), rc)
	if first == nil {
		t.Fatal("nil first pick")
	}
	first.MarkDegraded(now.Add(time.Hour))
	got := p.Pick(context.Background(), rc)
	if got == nil || got == first {
		t.Errorf("expected fallthrough to a different usable key, got %+v", got)
	}
}

func TestKeyPool_SnapshotMatchesKeys(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "round_robin")
	snaps := p.Snapshot()
	if len(snaps) != 2 {
		t.Errorf("len = %d, want 2", len(snaps))
	}
	if snaps[0].EnvVar != "A" || snaps[1].EnvVar != "B" {
		t.Errorf("snapshot env vars = %v, %v", snaps[0].EnvVar, snaps[1].EnvVar)
	}
}

func TestKeyPool_NilSafe(t *testing.T) {
	t.Parallel()
	var p *KeyPool
	if p.Len() != 0 {
		t.Errorf("nil Len = %d", p.Len())
	}
	if p.Pick(context.Background(), RequestContext{Now: time.Now()}) != nil {
		t.Errorf("nil Pick should be nil")
	}
	if p.AnyUsable(time.Now()) {
		t.Errorf("nil AnyUsable should be false")
	}
}

func TestIsValidRotationPolicy(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "round_robin", "lru", "sticky_session", "single"} {
		if !IsValidRotationPolicy(name) {
			t.Errorf("expected %q valid", name)
		}
	}
	for _, name := range []string{"bogus", "ROUND_ROBIN", "rr"} {
		if IsValidRotationPolicy(name) {
			t.Errorf("expected %q invalid", name)
		}
	}
}
