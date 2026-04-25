package gateway

import (
	"sync"
	"time"
)

// readIndexTTL is the per-session inactivity window after which a
// session's seen-key map is dropped. A new request from that session
// id starts fresh, so re-read counts are scoped to a contiguous run
// of activity rather than spanning days. SPEC §9 default.
const readIndexTTL = 10 * time.Minute

// readIndexCap caps the number of concurrently-tracked sessions per
// gateway. On overflow, the oldest by last-seen timestamp is evicted.
// 256 is generous for single-user workflows; the gateway is not
// designed to multiplex hundreds of clients.
const readIndexCap = 256

// readIndex tracks (session_id → canonical_tool_arg → occurrence_count)
// to feed RepeatReadsInfo on each gateway request. One instance per
// Router (= per gateway), aligned with summarizerDedup's lifetime.
//
// Semantics pin (mirrors SPEC §4.5 / §7.6):
//
//   - For each request, observe(sessionID, keys) returns:
//     count        = number of DISTINCT keys in this request that
//     were already seen in an earlier request of the
//     same session. Intra-request duplicates do not
//     inflate this number.
//     maxSamePath  = max TOTAL occurrence count of any single key
//     across all turns of the session, counting every
//     occurrence including intra-turn duplicates.
//
//   - State expires after readIndexTTL of inactivity per session.
//     Capacity-bound at readIndexCap sessions; oldest evicted on
//     overflow.
//
//   - All public operations are safe under concurrent access.
type readIndex struct {
	mu       sync.Mutex
	sessions map[string]*sessionReadState
	clock    func() time.Time
}

type sessionReadState struct {
	// counts maps canonical tool args to total occurrences across
	// every turn of this session.
	counts map[string]int
	// lastSeen advances on every observe; older sessions evict first.
	lastSeen time.Time
}

func newReadIndex() *readIndex {
	return &readIndex{
		sessions: make(map[string]*sessionReadState),
		clock:    time.Now,
	}
}

// observe records the canonical tool args for the current request and
// returns the (count, maxSamePath) pair per the SPEC contract above.
// An empty sessionID or empty keys slice yields (0, 0) without
// touching the index — callers treat both as "skip".
func (r *readIndex) observe(sessionID string, keys []string) (count, maxSamePath int) {
	if sessionID == "" || len(keys) == 0 {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock()
	state := r.getOrCreateLocked(sessionID, now)

	// count: distinct keys present in BOTH this request AND the
	// pre-existing state.counts. Intra-request duplicates contribute
	// once.
	seenThisRequest := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, dup := seenThisRequest[k]; dup {
			continue
		}
		seenThisRequest[k] = struct{}{}
		if _, existed := state.counts[k]; existed {
			count++
		}
	}

	// Update counts: every occurrence (including intra-turn dupes)
	// adds to the per-key tally.
	for _, k := range keys {
		if k == "" {
			continue
		}
		state.counts[k]++
	}

	// maxSamePath after the update.
	for _, n := range state.counts {
		if n > maxSamePath {
			maxSamePath = n
		}
	}

	state.lastSeen = now
	return count, maxSamePath
}

// getOrCreateLocked returns the per-session state, creating a fresh
// one when the session is new or its last activity exceeded
// readIndexTTL. Caller must hold r.mu.
func (r *readIndex) getOrCreateLocked(sessionID string, now time.Time) *sessionReadState {
	if state, ok := r.sessions[sessionID]; ok {
		if now.Sub(state.lastSeen) <= readIndexTTL {
			return state
		}
		// Stale — recycle the entry rather than allocating a new one.
		state.counts = make(map[string]int)
		state.lastSeen = now
		return state
	}
	if len(r.sessions) >= readIndexCap {
		r.evictOldestLocked()
	}
	s := &sessionReadState{
		counts:   make(map[string]int),
		lastSeen: now,
	}
	r.sessions[sessionID] = s
	return s
}

// evictOldestLocked drops the single least-recently-seen session.
// Called only when the map is at cap. Caller must hold r.mu.
func (r *readIndex) evictOldestLocked() {
	var oldestKey string
	var oldestTS time.Time
	for k, s := range r.sessions {
		if oldestKey == "" || s.lastSeen.Before(oldestTS) {
			oldestKey = k
			oldestTS = s.lastSeen
		}
	}
	delete(r.sessions, oldestKey)
}
