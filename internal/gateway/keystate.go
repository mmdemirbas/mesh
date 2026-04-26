package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Workstream A — per-key state, key pool, and rotation policy.
//
// This file introduces the data model A.1+ hooks into. Behavior is
// unchanged from the single-key world: NewKeyPool(apiKey, ...) with a
// non-empty key returns a one-element pool that always picks the same
// key. NewKeyPool with no keys returns a pool whose Pick() returns nil
// (passthrough mode where the client supplies its own auth).
//
// See docs/gateway/DESIGN_WORKSTREAM_A.local.md §3 for the state model
// and §2 for the rotation policy interface.

// KeyState is the runtime state of one upstream API key. Identifier is
// derived from the env-var name (or "default" for the legacy single-
// key case); Value is the resolved env-var value at startup. State
// fields are mutated under mu.
type KeyState struct {
	// ID is a stable, log-safe identifier for this key. Built from
	// the env-var name plus the last-4 of the sha256 of the secret
	// value so the operator can correlate "which key" without ever
	// seeing the secret. Empty Value yields ID "default" for the
	// legacy passthrough case.
	ID string

	// EnvVar is the environment variable name that supplied the
	// secret. Empty when the operator passed a literal value or no
	// key (passthrough). Used by validation error messages.
	EnvVar string

	// Value is the resolved secret. Never logged. Read-only after
	// construction.
	Value string

	mu sync.Mutex
	// degradedUntil is the wall clock time when this key recovers
	// from a 429/5xx-induced backoff. Zero = healthy.
	degradedUntil time.Time
	// successes / failures since process start. The thresholds for
	// passive health detection live on the upstream config; counters
	// here are the raw signal.
	successes int64
	failures  int64
	lastUsed  time.Time
}

// NewKeyState constructs a KeyState from an env var name + resolved
// secret. Empty value is allowed (passthrough mode); the resulting
// state is treated as "no key" by the rotation policy.
func NewKeyState(envVar, value string) *KeyState {
	return &KeyState{
		ID:     keyIDFor(envVar, value),
		EnvVar: envVar,
		Value:  value,
	}
}

// keyIDFor builds the stable, log-safe identifier per §5 of the
// design ("last-4-of-hash"). Format:
//
//	<envvar>:<last4>   when both fields are present
//	<envvar>:empty     when env is set but value is empty
//	default            when no env and no value (legacy passthrough)
//	literal:<last4>    when value is set but env name is unknown
func keyIDFor(envVar, value string) string {
	if value == "" {
		if envVar != "" {
			return envVar + ":empty"
		}
		return "default"
	}
	sum := sha256.Sum256([]byte(value))
	last4 := hex.EncodeToString(sum[:])[60:64]
	if envVar != "" {
		return envVar + ":" + last4
	}
	return "literal:" + last4
}

// IsUsable reports whether this key is currently safe to dispatch
// traffic to. False when the key's degradedUntil window has not yet
// elapsed; true otherwise (including the steady state where
// degradedUntil is zero).
func (k *KeyState) IsUsable(now time.Time) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.degradedUntil.IsZero() {
		return true
	}
	return !now.Before(k.degradedUntil)
}

// MarkSuccess records a successful response and clears any
// degradation state. Safe to call concurrently.
func (k *KeyState) MarkSuccess(now time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.successes++
	k.degradedUntil = time.Time{}
	k.lastUsed = now
	k.mu.Unlock()
}

// MarkFailure increments the failure counter. The caller decides
// whether to also degrade the key via MarkDegraded (e.g., a transient
// 5xx might increment failures without immediate degradation).
func (k *KeyState) MarkFailure(now time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.failures++
	k.lastUsed = now
	k.mu.Unlock()
}

// MarkDegraded sets the degraded-until clock. If the key is already
// degraded for longer than `until`, the longer window wins (don't
// shorten an existing backoff). Safe to call concurrently.
func (k *KeyState) MarkDegraded(until time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.degradedUntil.IsZero() || until.After(k.degradedUntil) {
		k.degradedUntil = until
	}
}

// MarkUsed updates the lastUsed clock without changing success/failure
// counters. Used by sticky_session and LRU policies that need a
// "this key was just dispatched to" signal independent of outcome.
func (k *KeyState) MarkUsed(now time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.lastUsed = now
	k.mu.Unlock()
}

// Snapshot copies the volatile fields under mu so callers (audit row
// builders, admin endpoints) can read without racing the dispatch
// goroutines.
type KeyStateSnapshot struct {
	ID            string
	EnvVar        string
	DegradedUntil time.Time
	Successes     int64
	Failures      int64
	LastUsed      time.Time
}

// Snapshot returns a value copy of the runtime fields.
func (k *KeyState) Snapshot() KeyStateSnapshot {
	if k == nil {
		return KeyStateSnapshot{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return KeyStateSnapshot{
		ID:            k.ID,
		EnvVar:        k.EnvVar,
		DegradedUntil: k.degradedUntil,
		Successes:     k.successes,
		Failures:      k.failures,
		LastUsed:      k.lastUsed,
	}
}
