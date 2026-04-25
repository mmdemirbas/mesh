package gateway

import (
	"sync"
	"testing"
	"time"
)

// fixedTime is a deterministic time generator. at(n) returns
// base + step*n. Used to bypass time.Now's non-determinism in unit
// tests.
type fixedTime struct {
	base time.Time
	step time.Duration
}

func newFixedTime(step time.Duration) *fixedTime {
	return &fixedTime{base: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), step: step}
}

func (ft *fixedTime) at(n int) time.Time { return ft.base.Add(ft.step * time.Duration(n)) }

func TestSegmentTimerMarkSequence(t *testing.T) {
	// Six Marks at evenly-spaced timestamps. Each named segment
	// should accumulate exactly one step's worth of duration.
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segClientToMesh, ft.at(0))
	tm.Mark(segMeshTranslationIn, ft.at(1))
	tm.Mark(segMeshToUpstream, ft.at(2))
	tm.Mark(segUpstreamProcessing, ft.at(3))
	tm.Mark(segMeshTranslationOut, ft.at(4))
	tm.Mark(segMeshToClient, ft.at(5))

	got := tm.Snapshot(ft.at(6))
	want := map[timingSegment]time.Duration{
		segClientToMesh:       10 * time.Millisecond,
		segMeshTranslationIn:  10 * time.Millisecond,
		segMeshToUpstream:     10 * time.Millisecond,
		segUpstreamProcessing: 10 * time.Millisecond,
		segMeshTranslationOut: 10 * time.Millisecond,
		segMeshToClient:       10 * time.Millisecond,
	}
	for _, seg := range orderedSegments {
		if got[seg] != want[seg] {
			t.Errorf("seg=%q got=%v want=%v", seg, got[seg], want[seg])
		}
	}
}

func TestSegmentTimerPauseClosesOpenSpan(t *testing.T) {
	ft := newFixedTime(5 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segUpstreamProcessing, ft.at(0))
	tm.Pause(ft.at(1))                           // closes upstream_processing at +5ms
	tm.Add(segMeshToClient, 30*time.Millisecond) // pure accumulator
	got := tm.Snapshot(ft.at(5))                 // 20ms after pause; nothing should accumulate

	if got[segUpstreamProcessing] != 5*time.Millisecond {
		t.Errorf("upstream_processing got=%v want=5ms", got[segUpstreamProcessing])
	}
	if got[segMeshToClient] != 30*time.Millisecond {
		t.Errorf("mesh_to_client got=%v want=30ms (Add only)", got[segMeshToClient])
	}
	if got[segMeshTranslationOut] != 0 {
		t.Errorf("mesh_translation_out got=%v want=0 (paused, no Mark/Add)", got[segMeshTranslationOut])
	}
}

func TestSegmentTimerAddIgnoresNonPositive(t *testing.T) {
	tm := newSegmentTimer()
	tm.Add(segClientToMesh, 0)
	tm.Add(segClientToMesh, -5*time.Millisecond)
	tm.Add(segClientToMesh, 1*time.Millisecond)
	tm.Add("", 100*time.Millisecond) // empty seg ignored

	got := tm.Snapshot(time.Now())
	if got[segClientToMesh] != 1*time.Millisecond {
		t.Errorf("client_to_mesh got=%v want=1ms", got[segClientToMesh])
	}
	if len(got) != 1 {
		t.Errorf("snapshot len got=%d want=1 (only client_to_mesh accumulated)", len(got))
	}
}

func TestSegmentTimerAddDoesNotAffectOpenSpan(t *testing.T) {
	// While segUpstreamProcessing is open, Add into segMeshTranslationOut
	// must not steal time from segUpstreamProcessing's running span.
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segUpstreamProcessing, ft.at(0))
	tm.Add(segMeshTranslationOut, 7*time.Millisecond)
	got := tm.Snapshot(ft.at(1)) // close at +10ms

	if got[segUpstreamProcessing] != 10*time.Millisecond {
		t.Errorf("upstream_processing got=%v want=10ms", got[segUpstreamProcessing])
	}
	if got[segMeshTranslationOut] != 7*time.Millisecond {
		t.Errorf("mesh_translation_out got=%v want=7ms", got[segMeshTranslationOut])
	}
}

func TestSegmentTimerSnapshotIsIdempotent(t *testing.T) {
	// Two Snapshot calls in a row should not double-count the open span.
	// (After the first Snapshot the timer is paused.)
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segClientToMesh, ft.at(0))
	first := tm.Snapshot(ft.at(1))
	second := tm.Snapshot(ft.at(5))

	if first[segClientToMesh] != 10*time.Millisecond {
		t.Errorf("first.client_to_mesh got=%v want=10ms", first[segClientToMesh])
	}
	if second[segClientToMesh] != 10*time.Millisecond {
		t.Errorf("second.client_to_mesh got=%v want=10ms (paused, no growth)", second[segClientToMesh])
	}
}

func TestSegmentTimerSnapshotEmpty(t *testing.T) {
	tm := newSegmentTimer()
	got := tm.Snapshot(time.Now())
	if len(got) != 0 {
		t.Errorf("empty timer snapshot len=%d want=0", len(got))
	}
}

func TestSegmentTimerMarkOverwritesOpenSpan(t *testing.T) {
	// Mark(segA) then Mark(segA) again at a later time should split
	// the duration cleanly across two contiguous spans of segA — i.e.
	// the second Mark closes the first span, the total duration is
	// preserved, but segA's accumulator only gets one final value.
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segUpstreamProcessing, ft.at(0))
	tm.Mark(segUpstreamProcessing, ft.at(1)) // re-mark same seg
	got := tm.Snapshot(ft.at(2))

	if got[segUpstreamProcessing] != 20*time.Millisecond {
		t.Errorf("upstream_processing got=%v want=20ms (two contiguous spans)", got[segUpstreamProcessing])
	}
}

func TestSegmentTimerPauseAfterPauseIsNoop(t *testing.T) {
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Pause(ft.at(0)) // pause with nothing open
	tm.Pause(ft.at(1)) // still nothing open
	got := tm.Snapshot(ft.at(2))

	if len(got) != 0 {
		t.Errorf("snapshot len=%d want=0", len(got))
	}
}

func TestSegmentTimerNegativeSpanIgnored(t *testing.T) {
	// If callbacks fire out of order (clock skew or unusual scheduler
	// behavior), the closeOpenLocked branch must not decrement the
	// accumulator.
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segClientToMesh, ft.at(2)) // open at +20ms
	got := tm.Snapshot(ft.at(0))       // close at +0ms — negative span
	if got[segClientToMesh] != 0 {
		t.Errorf("client_to_mesh got=%v want=0 (negative span ignored)", got[segClientToMesh])
	}
}

func TestSegmentTimerConcurrent(t *testing.T) {
	// Run with -race. Multiple goroutines hammer Mark/Add/Snapshot;
	// the only requirement is no race. We do not assert specific
	// values here because the interleaving is non-deterministic.
	tm := newSegmentTimer()
	var wg sync.WaitGroup
	const goroutines = 8
	const iters = 1000

	wg.Add(goroutines)
	for g := range goroutines {
		go func(seed int) {
			defer wg.Done()
			for i := range iters {
				switch (seed + i) % 4 {
				case 0:
					tm.Mark(segUpstreamProcessing, time.Now())
				case 1:
					tm.Add(segMeshTranslationOut, time.Microsecond)
				case 2:
					tm.Pause(time.Now())
				case 3:
					_ = tm.Snapshot(time.Now())
				}
			}
		}(g)
	}
	wg.Wait()

	// Final snapshot just to confirm no panic.
	_ = tm.Snapshot(time.Now())
}

func TestSegmentTimerSnapshotReturnsCopy(t *testing.T) {
	// Mutating the returned map must not affect subsequent snapshots.
	ft := newFixedTime(10 * time.Millisecond)
	tm := newSegmentTimer()

	tm.Mark(segClientToMesh, ft.at(0))
	first := tm.Snapshot(ft.at(1))
	first[segClientToMesh] = 9999 * time.Hour
	first[segMeshToUpstream] = 9999 * time.Hour

	second := tm.Snapshot(ft.at(2))
	if second[segClientToMesh] != 10*time.Millisecond {
		t.Errorf("second.client_to_mesh got=%v want=10ms (mutation of first leaked)", second[segClientToMesh])
	}
	if second[segMeshToUpstream] != 0 {
		t.Errorf("second.mesh_to_upstream got=%v want=0 (mutation of first leaked)", second[segMeshToUpstream])
	}
}
