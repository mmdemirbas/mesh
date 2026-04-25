package gateway

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestReadIndex_SPECFixtureA_TwoTurnSameKey is the §7.6 (a) fixture:
// two-request session, both Read(/foo). Turn 2 reports count=1,
// max_same_path=2.
func TestReadIndex_SPECFixtureA_TwoTurnSameKey(t *testing.T) {
	t.Parallel()
	r := newReadIndex()

	c1, m1 := r.observe("sess", []string{"/foo"})
	if c1 != 0 || m1 != 1 {
		t.Errorf("turn 1: count=%d max=%d, want 0,1", c1, m1)
	}
	c2, m2 := r.observe("sess", []string{"/foo"})
	if c2 != 1 || m2 != 2 {
		t.Errorf("turn 2: count=%d max=%d, want 1,2", c2, m2)
	}
}

// TestReadIndex_SPECFixtureB_IntraTurnDuplicates is the §7.6 (b)
// fixture: turns 1, 2 each Read(/foo); turn 3 reads /foo twice in
// the same turn. Turn 3: count=1 (one distinct key seen earlier),
// max_same_path=4 (every occurrence including intra-turn dupes).
func TestReadIndex_SPECFixtureB_IntraTurnDuplicates(t *testing.T) {
	t.Parallel()
	r := newReadIndex()

	r.observe("sess", []string{"/foo"})
	r.observe("sess", []string{"/foo"})
	c3, m3 := r.observe("sess", []string{"/foo", "/foo"})
	if c3 != 1 {
		t.Errorf("turn 3 count = %d, want 1 (one distinct key seen earlier)", c3)
	}
	if m3 != 4 {
		t.Errorf("turn 3 max_same_path = %d, want 4 (4 total occurrences of /foo)", m3)
	}
}

// TestReadIndex_SPECFixtureC_DistinctKeysVsTotalOccurrences is the
// §7.6 (c) fixture that pins the distinct-keys-vs-total-occurrences
// distinction. Turn 3 reads /foo (last seen turn 1) AND /bar (last
// seen turn 2). count=2 (both seen earlier), max_same_path=2.
func TestReadIndex_SPECFixtureC_DistinctKeysVsTotalOccurrences(t *testing.T) {
	t.Parallel()
	r := newReadIndex()

	r.observe("sess", []string{"/foo"})
	r.observe("sess", []string{"/bar"})
	c3, m3 := r.observe("sess", []string{"/foo", "/bar"})
	if c3 != 2 {
		t.Errorf("turn 3 count = %d, want 2 (two distinct previously-seen keys)", c3)
	}
	if m3 != 2 {
		t.Errorf("turn 3 max_same_path = %d, want 2", m3)
	}
}

// TestReadIndex_NewKeyInLaterTurnDoesNotCount: a key that appears
// only in this request (never before) is not "repeat" → count
// excludes it.
func TestReadIndex_NewKeyInLaterTurnDoesNotCount(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	r.observe("sess", []string{"/foo"})
	c, m := r.observe("sess", []string{"/bar"})
	if c != 0 {
		t.Errorf("count = %d, want 0 (/bar was never seen before)", c)
	}
	if m != 1 {
		t.Errorf("max_same_path = %d, want 1", m)
	}
}

// TestReadIndex_TTLExpirationStartsFresh: a session inactive longer
// than readIndexTTL is treated as new — its key history is wiped.
// This protects against stale counts on long-idle sessions.
func TestReadIndex_TTLExpirationStartsFresh(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	now := time.Now()
	r.clock = func() time.Time { return now }

	r.observe("sess", []string{"/foo"})

	// Advance past TTL.
	now = now.Add(readIndexTTL + time.Second)

	c, m := r.observe("sess", []string{"/foo"})
	if c != 0 {
		t.Errorf("after TTL: count = %d, want 0 (history reset)", c)
	}
	if m != 1 {
		t.Errorf("after TTL: max_same_path = %d, want 1", m)
	}
}

// TestReadIndex_TTLBoundaryStillFreshAtExactlyTTL: at exactly
// readIndexTTL elapsed, the session is still fresh (the implementation
// uses `<=` not `<`).
func TestReadIndex_TTLBoundaryStillFreshAtExactlyTTL(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	now := time.Now()
	r.clock = func() time.Time { return now }

	r.observe("sess", []string{"/foo"})
	now = now.Add(readIndexTTL)
	c, _ := r.observe("sess", []string{"/foo"})
	if c != 1 {
		t.Errorf("at exactly TTL: count = %d, want 1 (still fresh)", c)
	}
}

// TestReadIndex_LRUEvictionAtCap: at readIndexCap distinct sessions,
// the next new session evicts the oldest by lastSeen. The remaining
// active session's history is preserved.
func TestReadIndex_LRUEvictionAtCap(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	base := time.Now()
	step := time.Millisecond
	now := base
	r.clock = func() time.Time { return now }

	// Fill to cap: each session active at a slightly later time so
	// there's a clear oldest.
	for i := 0; i < readIndexCap; i++ {
		now = base.Add(time.Duration(i) * step)
		r.observe(fmt.Sprintf("sess-%04d", i), []string{"/x"})
	}
	if got := len(r.sessions); got != readIndexCap {
		t.Fatalf("len(sessions)=%d, want %d", got, readIndexCap)
	}

	// Trigger one more — should evict sess-0000 (oldest).
	now = base.Add(time.Duration(readIndexCap) * step)
	r.observe("sess-new", []string{"/y"})

	if got := len(r.sessions); got != readIndexCap {
		t.Errorf("after eviction len(sessions)=%d, want %d", got, readIndexCap)
	}
	if _, present := r.sessions["sess-0000"]; present {
		t.Errorf("sess-0000 should have been evicted as oldest")
	}
	if _, present := r.sessions["sess-new"]; !present {
		t.Errorf("sess-new should be present")
	}
}

// TestReadIndex_EmptyKeysIsNoOp: an empty keys slice yields
// (0, 0) and does NOT touch the index. (No artificial bumping of
// session lastSeen for a request that contributed no canonical keys.)
func TestReadIndex_EmptyKeysIsNoOp(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	c, m := r.observe("sess", nil)
	if c != 0 || m != 0 {
		t.Errorf("empty keys: count=%d max=%d, want 0,0", c, m)
	}
	if _, present := r.sessions["sess"]; present {
		t.Errorf("empty keys must not register a session entry")
	}
}

// TestReadIndex_EmptySessionIDIsNoOp: matches the empty-keys behavior
// for clients that send no session id at all.
func TestReadIndex_EmptySessionIDIsNoOp(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	c, m := r.observe("", []string{"/foo"})
	if c != 0 || m != 0 {
		t.Errorf("empty session: count=%d max=%d, want 0,0", c, m)
	}
}

// TestReadIndex_SessionIsolation: keys observed under one session
// id never affect another session's counts.
func TestReadIndex_SessionIsolation(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	r.observe("sess-a", []string{"/foo"})
	c, m := r.observe("sess-b", []string{"/foo"})
	if c != 0 {
		t.Errorf("session-b count = %d, want 0 (sess-a's history is isolated)", c)
	}
	if m != 1 {
		t.Errorf("session-b max_same_path = %d, want 1", m)
	}
}

// TestReadIndex_RaceSafety stresses concurrent observes on the same
// session id. Final state should reflect every increment exactly
// once; max_same_path equals the total observation count.
func TestReadIndex_RaceSafety(t *testing.T) {
	t.Parallel()
	r := newReadIndex()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			r.observe("sess", []string{"/foo"})
		}()
	}
	wg.Wait()

	r.mu.Lock()
	state := r.sessions["sess"]
	count := state.counts["/foo"]
	r.mu.Unlock()

	if count != N {
		t.Errorf("after %d concurrent observes: count[/foo]=%d, want %d", N, count, N)
	}
}
