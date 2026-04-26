package gateway

import (
	"sync"
	"sync/atomic"
	"time"
)

// B4 active-request registry.
//
// One process-wide ActiveRegistry tracks every request currently in
// flight through any wrapAuditing-wrapped gateway. The admin server
// reads it for the chrome indicator and the per-request live view.
// Lifecycle hooks in audit.go register/unregister entries; phase
// transitions and byte counters update in place via the existing
// segmentTimer.Mark sites and the auditingWriter Write hook.
//
// Concurrency:
//
//   - registryMu guards entries map (insert / delete / iterate).
//     RLock for reads, Lock for writes.
//   - Per-entry mutating fields use sync.Mutex (CurrentSegment,
//     SegmentStartedAt) for atomicity across reads. Byte counters
//     use atomic.Int64 — incremented in hot streaming loops without
//     locking.
//
// See docs/gateway/DESIGN_B4_live_tail.local.md.

// ActiveRequest is one in-flight request's live state. Pointer
// semantics: registry callers update fields via the receiver
// methods (mutex-protected for non-atomic fields, atomic for byte
// counters).
type ActiveRequest struct {
	ID          uint64
	Gateway     string
	SessionID   string
	ClientModel string
	Streaming   bool
	StartedAt   time.Time

	// Phase fields and UpstreamModel — protected by phaseMu.
	// UpstreamModel is mutable post-Register (resolved when routing
	// lands), unlike the other identity fields which are write-once
	// before Register, so it shares phaseMu's coverage. Without this
	// the SetUpstreamModel writer would race with snapshotOf readers
	// (deep-review B2).
	phaseMu          sync.Mutex
	upstreamModel    string
	currentSegment   string
	segmentStartedAt time.Time

	// Byte counters — atomic, no lock needed.
	bytesUpstream   atomic.Int64
	bytesDownstream atomic.Int64
	bytesToClient   atomic.Int64
}

// CurrentSegment returns the currently-open phase. Safe for
// concurrent callers.
func (a *ActiveRequest) CurrentSegment() string {
	a.phaseMu.Lock()
	defer a.phaseMu.Unlock()
	return a.currentSegment
}

// SegmentStartedAt returns when the current phase opened. Safe for
// concurrent callers.
func (a *ActiveRequest) SegmentStartedAt() time.Time {
	a.phaseMu.Lock()
	defer a.phaseMu.Unlock()
	return a.segmentStartedAt
}

// BytesUpstream / BytesDownstream / BytesToClient are atomic loads.
func (a *ActiveRequest) BytesUpstream() int64   { return a.bytesUpstream.Load() }
func (a *ActiveRequest) BytesDownstream() int64 { return a.bytesDownstream.Load() }
func (a *ActiveRequest) BytesToClient() int64   { return a.bytesToClient.Load() }

// updatePhase moves the request to a new segment. Called from
// wrapAuditing's Mark sites and from segment_timer's httptrace
// callbacks. Safe for concurrent callers.
func (a *ActiveRequest) updatePhase(seg string, when time.Time) {
	a.phaseMu.Lock()
	a.currentSegment = seg
	a.segmentStartedAt = when
	a.phaseMu.Unlock()
}

// addBytesUpstream / addBytesDownstream / addBytesToClient are
// hot-path accumulators called from streaming loops and the
// auditingWriter. Atomic, lock-free.
func (a *ActiveRequest) addBytesUpstream(n int64)   { a.bytesUpstream.Add(n) }
func (a *ActiveRequest) addBytesDownstream(n int64) { a.bytesDownstream.Add(n) }
func (a *ActiveRequest) addBytesToClient(n int64)   { a.bytesToClient.Add(n) }

// ActiveRegistry tracks every wrapAuditing request currently in
// flight. Process-wide singleton via package-level Active; tests
// construct their own via newActiveRegistry for isolation.
type ActiveRegistry struct {
	mu      sync.RWMutex
	entries map[uint64]*ActiveRequest
}

// Active is the process-wide registry. wrapAuditing writes to it;
// the admin server reads it.
var Active = newActiveRegistry()

func newActiveRegistry() *ActiveRegistry {
	return &ActiveRegistry{entries: make(map[uint64]*ActiveRequest)}
}

// Register inserts a new entry for reqID and returns the pointer so
// callers can update fields directly. Safe to call with reqID = 0
// (no-op recorder) — returns nil and does nothing.
func (r *ActiveRegistry) Register(req *ActiveRequest) *ActiveRequest {
	if r == nil || req == nil || req.ID == 0 {
		return req
	}
	r.mu.Lock()
	r.entries[req.ID] = req
	r.mu.Unlock()
	return req
}

// Unregister removes the entry for reqID. Safe to call with an id
// that was never registered (no-op).
func (r *ActiveRegistry) Unregister(reqID uint64) {
	if r == nil || reqID == 0 {
		return
	}
	r.mu.Lock()
	delete(r.entries, reqID)
	r.mu.Unlock()
}

// UpdatePhase advances the request's current segment. Safe with an
// unknown reqID (no-op). Called from wrapAuditing and segment_timer
// at every existing timer.Mark site.
func (r *ActiveRegistry) UpdatePhase(reqID uint64, seg string, when time.Time) {
	if r == nil || reqID == 0 {
		return
	}
	r.mu.RLock()
	entry := r.entries[reqID]
	r.mu.RUnlock()
	if entry != nil {
		entry.updatePhase(seg, when)
	}
}

// AddBytesUpstream / AddBytesDownstream / AddBytesToClient bump the
// per-request byte counters. Called from the streaming loops and
// the auditingWriter Write hook. Hot path — atomic, no map lock
// after the initial pointer fetch.
func (r *ActiveRegistry) AddBytesUpstream(reqID uint64, n int64) {
	if entry := r.entryFor(reqID); entry != nil {
		entry.addBytesUpstream(n)
	}
}
func (r *ActiveRegistry) AddBytesDownstream(reqID uint64, n int64) {
	if entry := r.entryFor(reqID); entry != nil {
		entry.addBytesDownstream(n)
	}
}
func (r *ActiveRegistry) AddBytesToClient(reqID uint64, n int64) {
	if entry := r.entryFor(reqID); entry != nil {
		entry.addBytesToClient(n)
	}
}

// SetUpstreamModel records the resolved upstream model name once
// the routing decision lands. Optional — wrapAuditing sets it
// alongside Register if the mapping is known. Mutates under
// phaseMu so concurrent snapshot readers see a consistent value.
func (r *ActiveRegistry) SetUpstreamModel(reqID uint64, model string) {
	if entry := r.entryFor(reqID); entry != nil {
		entry.phaseMu.Lock()
		entry.upstreamModel = model
		entry.phaseMu.Unlock()
	}
}

func (r *ActiveRegistry) entryFor(reqID uint64) *ActiveRequest {
	if r == nil || reqID == 0 {
		return nil
	}
	r.mu.RLock()
	entry := r.entries[reqID]
	r.mu.RUnlock()
	return entry
}

// Snapshot returns a deep-enough copy of every active request for
// the admin /api/gateway/active endpoint. Each returned
// ActiveRequest is a fresh struct (so callers can mutate without
// affecting the registry); byte counter values are sampled at
// snapshot time.
func (r *ActiveRegistry) Snapshot() []ActiveRequestSnapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ActiveRequestSnapshot, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, snapshotOf(e))
	}
	return out
}

// SnapshotByID returns one entry by id, or (zero, false) when the
// request has completed (and was removed from the registry).
// Used by the per-request SSE handler.
func (r *ActiveRegistry) SnapshotByID(reqID uint64) (ActiveRequestSnapshot, bool) {
	if r == nil || reqID == 0 {
		return ActiveRequestSnapshot{}, false
	}
	r.mu.RLock()
	e, ok := r.entries[reqID]
	r.mu.RUnlock()
	if !ok {
		return ActiveRequestSnapshot{}, false
	}
	return snapshotOf(e), true
}

// Len returns the current number of active entries. Test helper +
// the §1.4 sanity-cap signal site.
func (r *ActiveRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// ActiveRequestSnapshot is the JSON-friendly value-typed view of an
// ActiveRequest at one moment. The admin endpoint serializes this
// directly; never share a pointer to the registry's live entry
// across the HTTP boundary.
type ActiveRequestSnapshot struct {
	ID               uint64    `json:"id"`
	Gateway          string    `json:"gateway"`
	SessionID        string    `json:"session_id,omitempty"`
	ClientModel      string    `json:"client_model,omitempty"`
	UpstreamModel    string    `json:"upstream_model,omitempty"`
	Streaming        bool      `json:"streaming"`
	StartedAt        time.Time `json:"started_at"`
	CurrentSegment   string    `json:"current_segment"`
	SegmentStartedAt time.Time `json:"segment_started_at"`
	BytesUpstream    int64     `json:"bytes_upstream"`
	BytesDownstream  int64     `json:"bytes_downstream"`
	BytesToClient    int64     `json:"bytes_to_client"`
}

func snapshotOf(e *ActiveRequest) ActiveRequestSnapshot {
	if e == nil {
		return ActiveRequestSnapshot{}
	}
	e.phaseMu.Lock()
	model := e.upstreamModel
	seg := e.currentSegment
	segAt := e.segmentStartedAt
	e.phaseMu.Unlock()
	return ActiveRequestSnapshot{
		ID:               e.ID,
		Gateway:          e.Gateway,
		SessionID:        e.SessionID,
		ClientModel:      e.ClientModel,
		UpstreamModel:    model,
		Streaming:        e.Streaming,
		StartedAt:        e.StartedAt,
		CurrentSegment:   seg,
		SegmentStartedAt: segAt,
		BytesUpstream:    e.bytesUpstream.Load(),
		BytesDownstream:  e.bytesDownstream.Load(),
		BytesToClient:    e.bytesToClient.Load(),
	}
}
