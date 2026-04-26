package gateway

import (
	"sync"
	"testing"
	"time"
)

func TestActiveRegistry_RegisterUnregister(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Register(&ActiveRequest{ID: 1, Gateway: "gw"})
	r.Register(&ActiveRequest{ID: 2, Gateway: "gw"})
	if r.Len() != 2 {
		t.Errorf("len = %d, want 2", r.Len())
	}
	r.Unregister(1)
	if r.Len() != 1 {
		t.Errorf("len = %d, want 1 after Unregister", r.Len())
	}
	if _, ok := r.SnapshotByID(1); ok {
		t.Errorf("SnapshotByID(1) should be missing")
	}
	if _, ok := r.SnapshotByID(2); !ok {
		t.Errorf("SnapshotByID(2) should be present")
	}
}

func TestActiveRegistry_ZeroIDIsNoOp(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Register(&ActiveRequest{ID: 0, Gateway: "x"})
	if r.Len() != 0 {
		t.Errorf("zero-id register should be a no-op, len=%d", r.Len())
	}
	r.UpdatePhase(0, "x", time.Now())
	r.AddBytesUpstream(0, 100)
	r.AddBytesDownstream(0, 100)
	r.AddBytesToClient(0, 100)
}

func TestActiveRegistry_UpdatePhase(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Register(&ActiveRequest{ID: 1})
	now := time.Now()
	r.UpdatePhase(1, "mesh_to_upstream", now)
	snap, ok := r.SnapshotByID(1)
	if !ok {
		t.Fatal("missing entry")
	}
	if snap.CurrentSegment != "mesh_to_upstream" {
		t.Errorf("CurrentSegment = %q, want mesh_to_upstream", snap.CurrentSegment)
	}
	if !snap.SegmentStartedAt.Equal(now) {
		t.Errorf("SegmentStartedAt mismatch: %v != %v", snap.SegmentStartedAt, now)
	}
}

func TestActiveRegistry_ByteCountersAtomic(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Register(&ActiveRequest{ID: 7})
	const goroutines = 50
	const perG = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				r.AddBytesUpstream(7, 1)
				r.AddBytesDownstream(7, 2)
				r.AddBytesToClient(7, 3)
			}
		}()
	}
	wg.Wait()
	snap, _ := r.SnapshotByID(7)
	want := int64(goroutines * perG)
	if snap.BytesUpstream != want {
		t.Errorf("BytesUpstream = %d, want %d", snap.BytesUpstream, want)
	}
	if snap.BytesDownstream != want*2 {
		t.Errorf("BytesDownstream = %d, want %d", snap.BytesDownstream, want*2)
	}
	if snap.BytesToClient != want*3 {
		t.Errorf("BytesToClient = %d, want %d", snap.BytesToClient, want*3)
	}
}

func TestActiveRegistry_SnapshotIsCopy(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Register(&ActiveRequest{ID: 1, Gateway: "gw"})
	r.AddBytesUpstream(1, 100)
	snap1 := r.Snapshot()
	r.AddBytesUpstream(1, 200) // mutates after snapshot
	snap2 := r.Snapshot()
	// snap1 captured 100; snap2 captured 300.
	if len(snap1) != 1 || snap1[0].BytesUpstream != 100 {
		t.Errorf("snap1 = %+v, want bytes=100", snap1)
	}
	if len(snap2) != 1 || snap2[0].BytesUpstream != 300 {
		t.Errorf("snap2 = %+v, want bytes=300", snap2)
	}
}

func TestActiveRegistry_NilReceiverNoPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if x := recover(); x != nil {
			t.Errorf("nil receiver panicked: %v", x)
		}
	}()
	var r *ActiveRegistry
	r.Register(&ActiveRequest{ID: 1})
	r.Unregister(1)
	r.UpdatePhase(1, "x", time.Now())
	r.AddBytesUpstream(1, 1)
	if got := r.Snapshot(); got != nil {
		t.Errorf("nil snapshot want nil, got %+v", got)
	}
	if r.Len() != 0 {
		t.Errorf("nil len want 0")
	}
	if _, ok := r.SnapshotByID(1); ok {
		t.Errorf("nil SnapshotByID should be missing")
	}
}

func TestActiveRegistry_UnregisterMissingIsNoOp(t *testing.T) {
	t.Parallel()
	r := newActiveRegistry()
	r.Unregister(42)
	if r.Len() != 0 {
		t.Errorf("len after unregister-missing = %d, want 0", r.Len())
	}
}
