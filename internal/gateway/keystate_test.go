package gateway

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKeyIDFor_FormatAndStability(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env, val string
		want     string
	}{
		{"", "", "default"},
		{"FOO", "", "FOO:empty"},
		{"FOO", "secret-value", ""}, // expect "FOO:<last4>", checked below
		{"", "lone-secret", ""},     // expect "literal:<last4>"
	}
	for _, c := range cases {
		got := keyIDFor(c.env, c.val)
		if c.want != "" && got != c.want {
			t.Errorf("keyIDFor(%q,%q) = %q, want %q", c.env, c.val, got, c.want)
			continue
		}
		// For the secret cases, just sanity-check shape.
		if c.want == "" {
			if c.env != "" && !strings.HasPrefix(got, c.env+":") {
				t.Errorf("keyIDFor(%q,%q) = %q, want prefix %q:", c.env, c.val, got, c.env)
			}
			if c.env == "" && !strings.HasPrefix(got, "literal:") {
				t.Errorf("keyIDFor(%q,%q) = %q, want literal: prefix", c.env, c.val, got)
			}
			// Last-4 of sha256 is 4 hex chars.
			parts := strings.SplitN(got, ":", 2)
			if len(parts) != 2 || len(parts[1]) != 4 {
				t.Errorf("keyIDFor(%q,%q) = %q, want suffix length 4", c.env, c.val, got)
			}
		}
		// Stability: same inputs → same output.
		if got != keyIDFor(c.env, c.val) {
			t.Errorf("keyIDFor not stable for (%q,%q)", c.env, c.val)
		}
	}
}

func TestKeyIDFor_DifferentSecretsDifferentIDs(t *testing.T) {
	t.Parallel()
	a := keyIDFor("FOO", "secret-a")
	b := keyIDFor("FOO", "secret-b")
	if a == b {
		t.Errorf("same env, different secrets, same id: %q", a)
	}
}

func TestKeyState_DegradationLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	k := NewKeyState("FOO", "secret")
	if !k.IsUsable(now) {
		t.Errorf("fresh key should be usable")
	}
	k.MarkDegraded(now.Add(60 * time.Second))
	if k.IsUsable(now) {
		t.Errorf("just-degraded key should be unusable")
	}
	if k.IsUsable(now.Add(30 * time.Second)) {
		t.Errorf("mid-window key should be unusable")
	}
	if !k.IsUsable(now.Add(61 * time.Second)) {
		t.Errorf("post-window key should be usable")
	}
	k.MarkSuccess(now.Add(70 * time.Second))
	if !k.IsUsable(now.Add(30 * time.Second)) {
		t.Errorf("MarkSuccess should clear degradation, key was usable at any time after")
	}
}

func TestKeyState_DegradeDoesNotShortenLongerWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	k := NewKeyState("E", "v")
	k.MarkDegraded(now.Add(5 * time.Minute))
	k.MarkDegraded(now.Add(10 * time.Second)) // shorter — must not shorten
	if k.IsUsable(now.Add(2 * time.Minute)) {
		t.Errorf("shorter degraded-until should not shorten existing 5-minute window")
	}
}

func TestKeyState_NilSafe(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil receiver panicked: %v", r)
		}
	}()
	var k *KeyState
	if k.IsUsable(time.Now()) {
		t.Errorf("nil should be unusable")
	}
	k.MarkSuccess(time.Now())
	k.MarkFailure(time.Now())
	k.MarkDegraded(time.Now())
	k.MarkUsed(time.Now())
	if got := k.Snapshot(); got != (KeyStateSnapshot{}) {
		t.Errorf("nil snapshot want zero, got %+v", got)
	}
}

func TestKeyState_SnapshotIsCopy(t *testing.T) {
	t.Parallel()
	k := NewKeyState("E", "v")
	k.MarkSuccess(time.Now())
	s1 := k.Snapshot()
	k.MarkFailure(time.Now())
	s2 := k.Snapshot()
	if s1.Failures == s2.Failures {
		t.Errorf("snapshot didn't capture point-in-time state")
	}
}

// TestKeyState_MarkDegradedIfConsecFailures_Atomicity is the
// REVIEW #3 regression. The previous recordPassiveOutcome did
// Snapshot().ConsecFailures + MarkDegraded as two separate locks; a
// racing MarkSuccess between them could clear consecFailures to zero
// yet still be degraded based on the stale snapshot. The atomic
// helper performs the read-and-degrade decision under a single lock,
// so a MarkSuccess that lands first wins.
func TestKeyState_MarkDegradedIfConsecFailures_Atomicity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 11, 0, 0, 0, time.UTC)
	k := NewKeyState("E", "v")

	// Build up to threshold-1 failures, then a success → consec=0.
	k.MarkFailure(now)
	k.MarkFailure(now)
	k.MarkSuccess(now)
	if k.Snapshot().ConsecFailures != 0 {
		t.Fatalf("setup invariant: success should reset consec, got %d", k.Snapshot().ConsecFailures)
	}
	// At consec=0, threshold=3 must NOT degrade.
	if k.MarkDegradedIfConsecFailures(3, now.Add(60*time.Second)) {
		t.Errorf("MarkDegradedIfConsecFailures must not degrade when consec < threshold")
	}
	if !k.IsUsable(now) {
		t.Errorf("key was wrongly degraded under threshold")
	}

	// Push consec back up to threshold and confirm it does degrade.
	k.MarkFailure(now)
	k.MarkFailure(now)
	k.MarkFailure(now)
	if !k.MarkDegradedIfConsecFailures(3, now.Add(60*time.Second)) {
		t.Errorf("MarkDegradedIfConsecFailures should degrade at threshold")
	}
	if k.IsUsable(now) {
		t.Errorf("key should be degraded after threshold crossed")
	}
}

// TestKeyState_MarkDegradedIfConsecFailures_NilOrZeroThreshold pins
// the no-op branches: nil receiver and threshold<=0.
func TestKeyState_MarkDegradedIfConsecFailures_NilOrZeroThreshold(t *testing.T) {
	t.Parallel()
	var nilK *KeyState
	if nilK.MarkDegradedIfConsecFailures(1, time.Now()) {
		t.Errorf("nil receiver must return false")
	}
	k := NewKeyState("E", "v")
	for i := 0; i < 5; i++ {
		k.MarkFailure(time.Now())
	}
	if k.MarkDegradedIfConsecFailures(0, time.Now().Add(time.Minute)) {
		t.Errorf("threshold=0 must return false (operator opt-out)")
	}
}

// TestKeyState_RaceSuccessVsDegradeCheck stresses the read-and-degrade
// path under concurrent MarkSuccess to catch the original TOCTOU. The
// invariant: if at the moment of the threshold check consec is below
// threshold, the key must NOT end up degraded by that call.
//
// Run with the race detector. With the pre-fix code (Snapshot +
// MarkDegraded under two locks) this test reliably trips even without
// -race because a success can land between the two acquisitions and
// still leave the key degraded. With the atomic helper, the
// invariant holds.
func TestKeyState_RaceSuccessVsDegradeCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	const goroutines = 32
	const iterations = 500
	var wg sync.WaitGroup
	for trial := 0; trial < iterations; trial++ {
		k := NewKeyState("E", "v")
		k.MarkFailure(now)
		k.MarkFailure(now)
		k.MarkFailure(now) // consec=3 = threshold
		wg.Add(goroutines)
		// Half the goroutines try to degrade; half mark success
		// concurrently.
		for i := 0; i < goroutines; i++ {
			i := i
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					k.MarkSuccess(now)
				} else {
					k.MarkDegradedIfConsecFailures(3, now.Add(60*time.Second))
				}
			}()
		}
		wg.Wait()
		// Final invariant: if any MarkSuccess landed last, consec is 0
		// and the key must be usable. We don't know who landed last,
		// so just assert internal consistency: degraded => consec >=
		// threshold at the moment of the degrade decision (which is
		// what the atomic helper guarantees).
		s := k.Snapshot()
		if s.ConsecFailures < 3 && !s.DegradedUntil.IsZero() {
			// This is only legal if a MarkSuccess landed AFTER the
			// degrade was committed (which is fine — it can't undo a
			// past degrade). But MarkSuccess clears degradedUntil, so
			// if degradedUntil is non-zero with consec=0, that's a bug.
			t.Errorf("trial %d: degraded with consec=%d (expected MarkSuccess to clear degradedUntil)", trial, s.ConsecFailures)
		}
	}
}

func TestKeyState_ConcurrentMarks(t *testing.T) {
	t.Parallel()
	k := NewKeyState("E", "v")
	const goroutines = 50
	const perG = 1000
	now := time.Now()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				k.MarkSuccess(now)
				k.MarkFailure(now)
			}
		}()
	}
	wg.Wait()
	s := k.Snapshot()
	if s.Successes != int64(goroutines*perG) || s.Failures != int64(goroutines*perG) {
		t.Errorf("counters drifted under concurrency: %+v", s)
	}
}
