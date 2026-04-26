package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestSessionKeyMap_EvictsAtCap is the REVIEW #6 regression. The
// previous map[string]string grew without bound, so a misconfigured
// agent loop emitting a fresh session id per request could leak
// memory until process restart. The cap should evict the
// least-recently-used binding when the cap is exceeded.
func TestSessionKeyMap_EvictsAtCap(t *testing.T) {
	t.Parallel()
	var m sessionKeyMap
	for i := 0; i < maxStickySessionEntries+500; i++ {
		m.set(fmt.Sprintf("sess-%d", i), "K")
	}
	if got := m.len(); got != maxStickySessionEntries {
		t.Errorf("len = %d, want %d (cap not enforced)", got, maxStickySessionEntries)
	}
	// The earliest-inserted entries are gone.
	if _, ok := m.get("sess-0"); ok {
		t.Errorf("sess-0 should have been evicted")
	}
	// The newest is still present.
	last := fmt.Sprintf("sess-%d", maxStickySessionEntries+500-1)
	if _, ok := m.get(last); !ok {
		t.Errorf("%s should still be present", last)
	}
}

// TestSessionKeyMap_GetBumpsLRU pins that get() touches recency, so a
// session that's actively used in a long-lived workload doesn't get
// evicted just because newer sessions arrived.
func TestSessionKeyMap_GetBumpsLRU(t *testing.T) {
	t.Parallel()
	var m sessionKeyMap
	for i := 0; i < maxStickySessionEntries; i++ {
		m.set(fmt.Sprintf("sess-%d", i), "K")
	}
	// Touch sess-0 so it's now MRU.
	if _, ok := m.get("sess-0"); !ok {
		t.Fatal("sess-0 should be present pre-eviction")
	}
	// Insert one more entry. The LRU victim should be sess-1 (next
	// oldest), not sess-0.
	m.set("sess-new", "K")
	if _, ok := m.get("sess-0"); !ok {
		t.Errorf("sess-0 was evicted despite being recently accessed (LRU bug)")
	}
	if _, ok := m.get("sess-1"); ok {
		t.Errorf("sess-1 should be the LRU victim")
	}
}

// TestSessionKeyMap_SetUpdatesAndBumps pins that set() on an existing
// key updates the value and bumps it to MRU rather than allocating a
// duplicate entry.
func TestSessionKeyMap_SetUpdatesAndBumps(t *testing.T) {
	t.Parallel()
	var m sessionKeyMap
	m.set("a", "K1")
	m.set("a", "K2")
	if v, _ := m.get("a"); v != "K2" {
		t.Errorf("get(a) = %q, want K2", v)
	}
	if m.len() != 1 {
		t.Errorf("len = %d, want 1 after set-update", m.len())
	}
}

// TestSessionKeyMap_ConcurrentEviction runs concurrent set/get with
// distinct session ids to exercise the eviction path under -race. No
// invariants beyond "doesn't panic and stays at or under cap."
func TestSessionKeyMap_ConcurrentEviction(t *testing.T) {
	t.Parallel()
	var m sessionKeyMap
	const goroutines = 16
	const perG = 5_000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				m.set(fmt.Sprintf("g%d-s%d", g, i), "K")
				m.get(fmt.Sprintf("g%d-s%d", g, i))
			}
		}()
	}
	wg.Wait()
	if got := m.len(); got > maxStickySessionEntries {
		t.Errorf("len = %d exceeds cap %d", got, maxStickySessionEntries)
	}
}

// TestStickySession_PolicyEvictsViaPool exercises the cap end-to-end
// through the rotation policy interface, ensuring the eviction logic
// is wired up the way stickySessionPolicy uses it.
func TestStickySession_PolicyEvictsViaPool(t *testing.T) {
	t.Parallel()
	p, _ := NewKeyPool([]string{"A", "B"}, []string{"a", "b"}, "sticky_session")
	now := time.Now()
	for i := 0; i < maxStickySessionEntries+200; i++ {
		rc := RequestContext{Now: now, SessionID: fmt.Sprintf("session-%d", i)}
		if k := p.Pick(context.Background(), rc); k == nil {
			t.Fatalf("pick %d nil", i)
		}
	}
	policy := p.Policy.(*stickySessionPolicy)
	if got := policy.assignments.len(); got > maxStickySessionEntries {
		t.Errorf("sticky policy retained %d entries, cap is %d", got, maxStickySessionEntries)
	}
}
