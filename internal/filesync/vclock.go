package filesync

import (
	"sort"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
)

// VectorClock is a per-file vector clock used for C6 conflict detection.
// Keys are device IDs (10-char Crockford base32, see deviceid.go). Values
// are monotonic per-device write counters. Absent entries are treated as
// zero. The zero value of VectorClock is a valid empty clock. See
// docs/filesync/DESIGN-v1.md §1.
type VectorClock map[string]uint64

// ClockOrder is the strict vector-clock ordering between two clocks.
type ClockOrder int

const (
	// ClockEqual: every entry matches.
	ClockEqual ClockOrder = iota
	// ClockBefore: a is dominated by b (a ≤ b and a ≠ b).
	ClockBefore
	// ClockAfter: a dominates b (a ≥ b and a ≠ b).
	ClockAfter
	// ClockConcurrent: neither dominates; a conflict must be resolved.
	ClockConcurrent
)

// bump increments self's counter by one. A nil or missing entry starts at
// zero, so the first bump puts it at 1. Returns a new map (does not alias
// the receiver) so callers may keep the prior vector if they need it.
func (v VectorClock) bump(self string) VectorClock {
	out := make(VectorClock, len(v)+1)
	for k, val := range v {
		out[k] = val
	}
	out[self] = out[self] + 1
	return out
}

// compareClocks returns the strict ordering between a and b. Missing keys
// are treated as zero.
func compareClocks(a, b VectorClock) ClockOrder {
	aDominates := false
	bDominates := false
	// Walk the union of keys. A single key where a[k] > b[k] means a has
	// news b has not seen; the opposite means b has news a has not seen.
	// Any single "both dominate" split → concurrent.
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	for k := range seen {
		av := a[k]
		bv := b[k]
		switch {
		case av > bv:
			aDominates = true
		case bv > av:
			bDominates = true
		}
		if aDominates && bDominates {
			return ClockConcurrent
		}
	}
	switch {
	case aDominates:
		return ClockAfter
	case bDominates:
		return ClockBefore
	default:
		return ClockEqual
	}
}

// toProto converts a VectorClock to the wire form, sorted by device_id so
// two semantically equal clocks serialize identically.
func (v VectorClock) toProto() []*pb.Counter {
	if len(v) == 0 {
		return nil
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*pb.Counter, 0, len(keys))
	for _, k := range keys {
		if v[k] == 0 {
			continue
		}
		out = append(out, &pb.Counter{DeviceId: k, Value: v[k]})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// vectorClockFromProto decodes a wire-form vector clock. Duplicate
// device_id entries keep the highest value (defensive; senders should
// deduplicate). Zero-valued entries are dropped.
func vectorClockFromProto(counters []*pb.Counter) VectorClock {
	if len(counters) == 0 {
		return nil
	}
	out := make(VectorClock, len(counters))
	for _, c := range counters {
		if c == nil || c.GetDeviceId() == "" || c.GetValue() == 0 {
			continue
		}
		if existing, ok := out[c.GetDeviceId()]; !ok || c.GetValue() > existing {
			out[c.GetDeviceId()] = c.GetValue()
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clone returns a deep copy. The receiver may be nil.
func (v VectorClock) clone() VectorClock {
	if len(v) == 0 {
		return nil
	}
	out := make(VectorClock, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}

// merge returns the per-component max of v and other, union of keys.
// Used when adopting a peer's clock after applying their write: we must
// preserve any local components the peer did not carry so our prior
// observations are not lost (e.g., during rolling upgrades when the
// peer ran a pre-C6 build and sent an empty clock). Zero values are
// dropped. Either side may be nil.
func (v VectorClock) merge(other VectorClock) VectorClock {
	if len(v) == 0 && len(other) == 0 {
		return nil
	}
	out := make(VectorClock, len(v)+len(other))
	for k, val := range v {
		if val > 0 {
			out[k] = val
		}
	}
	for k, val := range other {
		if val > out[k] {
			out[k] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
