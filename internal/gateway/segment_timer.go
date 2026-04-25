package gateway

import (
	"maps"
	"sync"
	"time"
)

// timingSegment identifies one of the six named segments in the §B1
// timing partition. The empty string is reserved for the "paused"
// state — see segmentTimer.Pause.
type timingSegment string

const (
	segClientToMesh       timingSegment = "client_to_mesh"
	segMeshTranslationIn  timingSegment = "mesh_translation_in"
	segMeshToUpstream     timingSegment = "mesh_to_upstream"
	segUpstreamProcessing timingSegment = "upstream_processing"
	segMeshTranslationOut timingSegment = "mesh_translation_out"
	segMeshToClient       timingSegment = "mesh_to_client"
)

// orderedSegments lists the six named segments in canonical order.
// Used by the row writer to emit a deterministic field ordering and
// by tests to enumerate without hardcoding the list.
var orderedSegments = [...]timingSegment{
	segClientToMesh,
	segMeshTranslationIn,
	segMeshToUpstream,
	segUpstreamProcessing,
	segMeshTranslationOut,
	segMeshToClient,
}

// segmentTimer accumulates per-segment wall-clock for one request.
//
// The timer holds time.Duration (nanosecond precision) per segment.
// Conversion to integer milliseconds happens once at row-emit time
// (DESIGN_B1_timing.local.md §10 D5) so the streaming loop's many
// Add calls do not lose sub-millisecond slices to per-call truncation.
//
// Mark/Pause/Add/Snapshot are concurrency-safe under sync.Mutex.
// httptrace.ClientTrace callbacks fire from net/http's internal
// goroutines, distinct from the goroutine running the request handler;
// the lock is required for correctness, not defensive (D6).
type segmentTimer struct {
	mu     sync.Mutex
	accums map[timingSegment]time.Duration
	open   timingSegment // "" when no span is open
	openAt time.Time     // valid only when open != ""
}

func newSegmentTimer() *segmentTimer {
	return &segmentTimer{accums: make(map[timingSegment]time.Duration, len(orderedSegments))}
}

// Mark closes the currently open span at when (if any), accumulating
// when - openAt into the previously open segment, and opens a new
// span keyed by seg starting at when. seg must be a non-empty named
// segment; pass "" via Pause instead.
func (t *segmentTimer) Mark(seg timingSegment, when time.Time) {
	if seg == "" {
		t.Pause(when)
		return
	}
	t.mu.Lock()
	t.closeOpenLocked(when)
	t.open = seg
	t.openAt = when
	t.mu.Unlock()
}

// Pause closes the currently open span at when without opening a new
// one. Used at streaming-loop entry where Add takes over per-phase
// accumulation.
func (t *segmentTimer) Pause(when time.Time) {
	t.mu.Lock()
	t.closeOpenLocked(when)
	t.open = ""
	t.mu.Unlock()
}

// Add accumulates d into seg without changing the open-span state.
// Negative or zero durations are ignored — accumulators only grow.
func (t *segmentTimer) Add(seg timingSegment, d time.Duration) {
	if seg == "" || d <= 0 {
		return
	}
	t.mu.Lock()
	t.accums[seg] += d
	t.mu.Unlock()
}

// Snapshot closes any open span at when and returns a copy of the
// per-segment accumulators. After Snapshot the timer is paused; a
// subsequent Mark would re-arm it, but the production path treats
// Snapshot as terminal.
func (t *segmentTimer) Snapshot(when time.Time) map[timingSegment]time.Duration {
	t.mu.Lock()
	t.closeOpenLocked(when)
	t.open = ""
	out := maps.Clone(t.accums)
	if out == nil {
		out = make(map[timingSegment]time.Duration)
	}
	t.mu.Unlock()
	return out
}

// closeOpenLocked accumulates the currently open span's duration up
// to when. Must be called with t.mu held. A negative duration (clock
// skew or out-of-order callback) is treated as zero — the named
// segment's accumulator never decreases.
func (t *segmentTimer) closeOpenLocked(when time.Time) {
	if t.open == "" || t.openAt.IsZero() {
		return
	}
	d := when.Sub(t.openAt)
	if d > 0 {
		t.accums[t.open] += d
	}
}
