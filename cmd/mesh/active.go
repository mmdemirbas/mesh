package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
)

// B4 admin endpoints — live in-flight request visibility.
//
// Two routes:
//
//   GET /api/gateway/active                    snapshot of all in-flight requests
//   GET /api/gateway/active/{id}/events        per-request SSE stream
//
// The snapshot endpoint matches the existing 1s polling rhythm of
// the chrome indicator. The SSE endpoint pushes every state change
// for one request, throttled to 4Hz to avoid event flooding on
// streams that produce hundreds of chunks per second.
//
// See docs/gateway/DESIGN_B4_live_tail.local.md.

// activePollInterval is the cadence at which the per-request SSE
// handler resamples the registry. 250 ms = 4 Hz, the §2.4 throttle.
const activePollInterval = 250 * time.Millisecond

// activeResponse is the shape returned by GET /api/gateway/active.
type activeResponse struct {
	AsOf      time.Time                       `json:"as_of"`
	Total     int                             `json:"total"`
	ByPhase   map[string]int                  `json:"by_phase"`
	ByGateway map[string]int                  `json:"by_gateway"`
	Requests  []gateway.ActiveRequestSnapshot `json:"requests"`
}

// handleActiveSnapshot serves /api/gateway/active. Reads from the
// process-wide gateway.Active registry, builds aggregate counts, and
// returns the JSON snapshot. No filtering — the operator's chrome
// shows the full live picture.
func handleActiveSnapshot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	snaps := gateway.Active.Snapshot()
	resp := activeResponse{
		AsOf:      time.Now(),
		Total:     len(snaps),
		ByPhase:   make(map[string]int, 7),
		ByGateway: make(map[string]int),
		Requests:  snaps,
	}
	for _, s := range snaps {
		seg := s.CurrentSegment
		if seg == "" {
			seg = "other"
		}
		resp.ByPhase[seg]++
		if s.Gateway != "" {
			resp.ByGateway[s.Gateway]++
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleActiveEvents is the per-request SSE handler. Streams `state`
// events at activePollInterval cadence whenever the entry's
// snapshot changes; sends a single `completed` event when the entry
// disappears from the registry, then closes.
func handleActiveEvents(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		http.Error(w, "id must be a positive integer", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Send the initial snapshot. If the request already completed,
	// emit `completed` and close.
	cur, found := gateway.Active.SnapshotByID(id)
	if !found {
		_ = emit("completed", map[string]any{"id": id, "reason": "already_completed"})
		return
	}
	if err := emit("state", cur); err != nil {
		return
	}

	serveActiveStream(r.Context(), id, cur, emit, activePollInterval)
}

// serveActiveStream is the testable poll loop. Re-samples the
// registry every poll interval; emits a `state` event when the
// snapshot has changed; emits `completed` and returns when the
// entry disappears.
func serveActiveStream(ctx context.Context, id uint64, initial gateway.ActiveRequestSnapshot, emit func(string, any) error, poll time.Duration) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	prev := initial
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// Comment-only heartbeat keeps middleboxes from idle-
			// timing-out a slow request's SSE connection.
			_ = emit("ping", "")
		case <-ticker.C:
			cur, found := gateway.Active.SnapshotByID(id)
			if !found {
				_ = emit("completed", map[string]any{"id": id})
				return
			}
			if !sameSnapshot(prev, cur) {
				if err := emit("state", cur); err != nil {
					return
				}
				prev = cur
			}
		}
	}
}

// sameSnapshot reports whether two snapshots describe identical
// state. Compares phase + byte counters; ignores AsOf-style fields
// since they always change. Used to skip redundant `state` events
// when nothing observable moved between ticks.
func sameSnapshot(a, b gateway.ActiveRequestSnapshot) bool {
	return a.CurrentSegment == b.CurrentSegment &&
		a.BytesUpstream == b.BytesUpstream &&
		a.BytesDownstream == b.BytesDownstream &&
		a.BytesToClient == b.BytesToClient &&
		a.UpstreamModel == b.UpstreamModel
}
