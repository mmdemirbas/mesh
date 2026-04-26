package gateway

import (
	"context"
	"time"
)

// KeyPool is the per-upstream collection of keys plus the rotation
// policy that picks one for a given request. A.0 ships single-key
// behavior unchanged (one-element pool, "single" policy that always
// returns the only key); A.1 adds true multi-key configs.
//
// See DESIGN_WORKSTREAM_A.local.md §2 (rotation policy) and §3 (per-
// key state).
type KeyPool struct {
	// Keys is the rotation pool. Non-nil but possibly empty for
	// passthrough upstreams that defer to client-supplied auth.
	Keys []*KeyState
	// Policy selects which key to use for a given request. nil means
	// "no policy needed" (zero-key passthrough or single-key default).
	Policy RotationPolicy
}

// NewKeyPool constructs a pool from the upstream's resolved key list.
// envVars + values must be the same length and order. An empty list
// produces a pool with zero keys (passthrough auth). A single-element
// list produces a pool with the singleKeyPolicy regardless of the
// configured policy name — single-key configs behave identically to
// the pre-A world.
func NewKeyPool(envVars, values []string, policyName string) (*KeyPool, error) {
	if len(envVars) != len(values) {
		// Programming error, not config error — the caller is
		// responsible for matched-length input. Panic-style return
		// communicates the contract violation without crashing.
		return nil, errInternalKeyMismatch{got: len(values), want: len(envVars)}
	}
	keys := make([]*KeyState, 0, len(values))
	for i, v := range values {
		var env string
		if i < len(envVars) {
			env = envVars[i]
		}
		keys = append(keys, NewKeyState(env, v))
	}
	if len(keys) <= 1 {
		// Single-key (or zero-key) pools always use the no-op
		// single policy — there is nothing to rotate. The
		// configured policy is preserved for documentation but
		// not executed.
		return &KeyPool{Keys: keys, Policy: singleKeyPolicy{}}, nil
	}
	policy, err := lookupRotationPolicy(policyName)
	if err != nil {
		return nil, err
	}
	return &KeyPool{Keys: keys, Policy: policy}, nil
}

// Pick selects the next usable key from the pool for the given
// request context. Returns nil when no key is currently usable
// (either the pool is empty or every key is degraded). The chain
// dispatcher (A.5) treats a nil pick as "advance to next upstream".
func (p *KeyPool) Pick(ctx context.Context, rc RequestContext) *KeyState {
	if p == nil || len(p.Keys) == 0 {
		return nil
	}
	if p.Policy == nil {
		// Defensive — NewKeyPool always sets a policy, but a
		// caller that constructs a KeyPool literal might not.
		return p.Keys[0]
	}
	return p.Policy.Next(p.Keys, rc)
}

// FirstUsable scans the pool and returns the first key whose
// IsUsable(now) is true, ignoring policy. Used by the admin "is
// this upstream usable at all" check (A.2 health), and by tests.
func (p *KeyPool) FirstUsable(now time.Time) *KeyState {
	if p == nil {
		return nil
	}
	for _, k := range p.Keys {
		if k.IsUsable(now) {
			return k
		}
	}
	return nil
}

// AnyUsable reports whether at least one key in the pool is
// currently usable. The chain dispatcher uses this as the per-
// upstream health gate (§3.2).
func (p *KeyPool) AnyUsable(now time.Time) bool {
	return p.FirstUsable(now) != nil
}

// Len returns the pool size (zero for passthrough, one for legacy
// single-key, more for multi-key).
func (p *KeyPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.Keys)
}

// Snapshot returns value copies of every key's runtime state for
// audit / admin surfaces.
func (p *KeyPool) Snapshot() []KeyStateSnapshot {
	if p == nil {
		return nil
	}
	out := make([]KeyStateSnapshot, 0, len(p.Keys))
	for _, k := range p.Keys {
		out = append(out, k.Snapshot())
	}
	return out
}

// errInternalKeyMismatch fires when NewKeyPool is called with
// mismatched envVars / values lengths. Programming error; the caller
// is responsible for matched input. Implements error.
type errInternalKeyMismatch struct {
	got, want int
}

func (e errInternalKeyMismatch) Error() string {
	return "keypool: internal length mismatch (envVars and values must be the same length)"
}
