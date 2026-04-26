package gateway

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// Workstream A — rotation policy registry.
//
// The set of policies mesh ships with. Each is named; the upstream
// config picks one by name; new policies can be added later. v1
// policies (per the design's likely set):
//
//   - single        — singleton pool; returns the only key. Used
//                     internally for single-key configs regardless
//                     of configured policy.
//   - round_robin   — simple cycle, even distribution.
//   - lru           — least-recently-used (or never-used) wins.
//   - sticky_session — same session id always uses the same key
//                     until it degrades.
//
// A.0 ships only `single`; A.1 adds round_robin/lru/sticky_session
// alongside the multi-key config plumbing.

// RotationPolicy picks a key from a pool given a request context.
// Returning nil means "no usable key" — the chain dispatcher treats
// that as "advance to next upstream."
type RotationPolicy interface {
	// Next picks the key to use for this request. May mutate
	// per-key bookkeeping fields (e.g., an LRU policy bumps
	// lastUsed before returning). Must return nil when no key
	// in pool is currently usable.
	Next(pool []*KeyState, rc RequestContext) *KeyState
	// Name is the policy's config-facing name (matches the YAML
	// `rotation_policy:` value).
	Name() string
}

// RequestContext carries the request-level signals rotation policies
// may consult. Session id for sticky policies, retry-attempt number
// for backoff-aware policies, request-time for LRU.
type RequestContext struct {
	SessionID    string
	AttemptIndex int
	Now          time.Time
}

// singleKeyPolicy returns the only key in a singleton pool. Internal:
// always selected when KeyPool.Len() <= 1, regardless of the
// configured policy name. This is the backward-compat path for
// single-key upstreams.
type singleKeyPolicy struct{}

func (singleKeyPolicy) Name() string { return "single" }
func (singleKeyPolicy) Next(pool []*KeyState, rc RequestContext) *KeyState {
	if len(pool) == 0 {
		return nil
	}
	if pool[0].IsUsable(rc.Now) {
		return pool[0]
	}
	return nil
}

// roundRobinPolicy cycles through the pool in order, skipping
// degraded keys. Distribution is even modulo the degradation
// pattern.
type roundRobinPolicy struct {
	cursor atomic.Uint64
}

func (r *roundRobinPolicy) Name() string { return "round_robin" }
func (r *roundRobinPolicy) Next(pool []*KeyState, rc RequestContext) *KeyState {
	if len(pool) == 0 {
		return nil
	}
	n := uint64(len(pool))
	for i := uint64(0); i < n; i++ {
		idx := (r.cursor.Add(1) - 1) % n
		if pool[idx].IsUsable(rc.Now) {
			return pool[idx]
		}
	}
	return nil
}

// lruPolicy picks the least-recently-used usable key. Ties broken by
// pool index (deterministic). Never-used keys (zero lastUsed) sort
// before any used key, matching the LRU intent of "warm a fresh key
// before reusing an old one."
type lruPolicy struct{}

func (lruPolicy) Name() string { return "lru" }
func (lruPolicy) Next(pool []*KeyState, rc RequestContext) *KeyState {
	type cand struct {
		k    *KeyState
		used time.Time
		idx  int
	}
	usable := make([]cand, 0, len(pool))
	for i, k := range pool {
		if !k.IsUsable(rc.Now) {
			continue
		}
		snap := k.Snapshot()
		usable = append(usable, cand{k, snap.LastUsed, i})
	}
	if len(usable) == 0 {
		return nil
	}
	sort.SliceStable(usable, func(i, j int) bool {
		// Zero time (never-used) sorts before any non-zero time.
		if usable[i].used.IsZero() != usable[j].used.IsZero() {
			return usable[i].used.IsZero()
		}
		if !usable[i].used.Equal(usable[j].used) {
			return usable[i].used.Before(usable[j].used)
		}
		return usable[i].idx < usable[j].idx
	})
	return usable[0].k
}

// stickySessionPolicy maps a session id to a key and reuses that
// assignment until the key degrades. On degradation, falls through
// to a fresh (usable) key for that session and updates the
// assignment. Cache-friendly default for prompt-cache-aware
// upstreams (Anthropic's prompt cache prefers requests sharing the
// same auth so the cache prefix is reused).
//
// A.1 wires this with a real assignment map; A.0 ships the type as
// a no-op fallthrough so the registry contract is honored.
type stickySessionPolicy struct {
	// assignments maps session id → keyID; rebuilt on degradation.
	// Sized: bounded by concurrent live sessions, which is small in
	// practice. No eviction needed for the in-process lifetime.
	assignments sessionKeyMap
}

func (*stickySessionPolicy) Name() string { return "sticky_session" }
func (s *stickySessionPolicy) Next(pool []*KeyState, rc RequestContext) *KeyState {
	if len(pool) == 0 {
		return nil
	}
	// If we have an assignment for this session and it's usable,
	// use it.
	if rc.SessionID != "" {
		if id, ok := s.assignments.get(rc.SessionID); ok {
			for _, k := range pool {
				if k.ID == id && k.IsUsable(rc.Now) {
					return k
				}
			}
		}
	}
	// Pick the first usable key, record the assignment.
	for _, k := range pool {
		if k.IsUsable(rc.Now) {
			if rc.SessionID != "" {
				s.assignments.set(rc.SessionID, k.ID)
			}
			return k
		}
	}
	return nil
}

// lookupRotationPolicy returns a fresh policy instance for the named
// policy. Each multi-key upstream gets its own instance so
// stateful policies (round-robin cursor, sticky-session map) don't
// leak across upstreams.
//
// Default when the name is empty is "round_robin" — the safest
// "spread load across configured keys" default for operators who
// add a second key without explicitly choosing a policy. The
// rationale (per design D5): single-key configs are unaffected
// (they short-circuit to singleKeyPolicy regardless), and operators
// who add a second key are signaling "I want load spread"; sticky
// is opt-in for the prompt-cache-aware case.
func lookupRotationPolicy(name string) (RotationPolicy, error) {
	switch name {
	case "", "round_robin":
		return &roundRobinPolicy{}, nil
	case "lru":
		return lruPolicy{}, nil
	case "sticky_session":
		return &stickySessionPolicy{}, nil
	case "single":
		return singleKeyPolicy{}, nil
	}
	return nil, fmt.Errorf("unknown rotation_policy %q (valid: round_robin, lru, sticky_session)", name)
}

// IsValidRotationPolicy reports whether name is one of the rotation
// policies mesh accepts in configuration. Empty is allowed (means
// "use default"). "single" is accepted but undocumented — it's the
// internal default for single-key pools.
func IsValidRotationPolicy(name string) bool {
	switch name {
	case "", "round_robin", "lru", "sticky_session", "single":
		return true
	}
	return false
}
