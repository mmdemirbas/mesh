package state

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newState() *State {
	return &State{components: make(map[string]Component)}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "127.0.0.1:1080", Listening, "ready")

	snap := s.Snapshot()
	comp, ok := snap["proxy:127.0.0.1:1080"]
	if !ok {
		t.Fatal("component not found after Update")
	}
	if comp.Type != "proxy" {
		t.Errorf("Type = %q, want %q", comp.Type, "proxy")
	}
	if comp.ID != "127.0.0.1:1080" {
		t.Errorf("ID = %q, want %q", comp.ID, "127.0.0.1:1080")
	}
	if comp.Status != Listening {
		t.Errorf("Status = %q, want %q", comp.Status, Listening)
	}
	if comp.Message != "ready" {
		t.Errorf("Message = %q, want %q", comp.Message, "ready")
	}
}

func TestUpdatePreservesBoundAddr(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "id1", Starting, "")
	s.UpdateBind("proxy", "id1", "127.0.0.1:9999")
	s.Update("proxy", "id1", Listening, "ok")

	snap := s.Snapshot()
	comp := snap["proxy:id1"]
	if comp.BoundAddr != "127.0.0.1:9999" {
		t.Errorf("BoundAddr = %q, want %q (should be preserved across Update)", comp.BoundAddr, "127.0.0.1:9999")
	}
	if comp.Status != Listening {
		t.Errorf("Status = %q, want %q", comp.Status, Listening)
	}
}

func TestUpdateBind(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("relay", "r1", Starting, "")
	s.UpdateBind("relay", "r1", "0.0.0.0:8080")

	snap := s.Snapshot()
	if snap["relay:r1"].BoundAddr != "0.0.0.0:8080" {
		t.Errorf("BoundAddr = %q, want %q", snap["relay:r1"].BoundAddr, "0.0.0.0:8080")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("server", "s1", Listening, "")
	s.Delete("server", "s1")

	snap := s.Snapshot()
	if _, ok := snap["server:s1"]; ok {
		t.Error("component still present after Delete")
	}
}

func TestDeleteNonExistent(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Delete("server", "nonexistent") // should not panic
}

func TestSnapshotIsACopy(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "p1", Listening, "")

	snap := s.Snapshot()
	snap["proxy:p1"] = Component{Message: "mutated"}

	snap2 := s.Snapshot()
	if snap2["proxy:p1"].Message == "mutated" {
		t.Error("Snapshot returned a reference, not a copy")
	}
}

func TestSnapshotEmpty(t *testing.T) {
	t.Parallel()
	s := newState()
	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot of empty state has %d entries", len(snap))
	}
}

func TestStatusConstants(t *testing.T) {
	t.Parallel()
	statuses := map[Status]string{
		Starting:   "starting",
		Listening:  "listening",
		Connecting: "connecting",
		Connected:  "connected",
		Failed:     "failed",
		Retrying:   "retrying",
	}
	for s, want := range statuses {
		if string(s) != want {
			t.Errorf("Status %v = %q, want %q", s, string(s), want)
		}
	}
}

func TestUpdatePeer(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("connection", "c1", Connected, "target1")
	s.UpdatePeer("connection", "c1", "10.0.0.1:22")

	snap := s.Snapshot()
	if snap["connection:c1"].PeerAddr != "10.0.0.1:22" {
		t.Errorf("PeerAddr = %q, want %q", snap["connection:c1"].PeerAddr, "10.0.0.1:22")
	}
}

func TestUpdatePreservesPeerAddr(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("connection", "c1", Connected, "target1")
	s.UpdatePeer("connection", "c1", "10.0.0.1:22")
	s.Update("connection", "c1", Retrying, "error")

	snap := s.Snapshot()
	if snap["connection:c1"].PeerAddr != "10.0.0.1:22" {
		t.Errorf("PeerAddr = %q, want %q (should be preserved across Update)", snap["connection:c1"].PeerAddr, "10.0.0.1:22")
	}
}

func TestGetMetrics_CreatesNew(t *testing.T) {
	t.Parallel()
	s := newState()
	m := s.GetMetrics("connection", "c1")
	if m == nil {
		t.Fatal("GetMetrics returned nil")
	}
	if m.BytesTx.Load() != 0 {
		t.Errorf("new metrics BytesTx = %d, want 0", m.BytesTx.Load())
	}
}

func TestGetMetrics_ReturnsSame(t *testing.T) {
	t.Parallel()
	s := newState()
	m1 := s.GetMetrics("connection", "c1")
	m2 := s.GetMetrics("connection", "c1")
	if m1 != m2 {
		t.Error("GetMetrics returned different pointers for same key")
	}
}

func TestGetMetrics_DifferentKeys(t *testing.T) {
	t.Parallel()
	s := newState()
	m1 := s.GetMetrics("connection", "c1")
	m2 := s.GetMetrics("connection", "c2")
	if m1 == m2 {
		t.Error("GetMetrics returned same pointer for different keys")
	}
}

func TestMetrics_AtomicOperations(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	m.BytesTx.Add(100)
	m.BytesTx.Add(200)
	m.BytesRx.Add(50)
	m.Streams.Add(3)
	m.Streams.Add(-1)
	m.StartTime.Store(12345)

	if m.BytesTx.Load() != 300 {
		t.Errorf("BytesTx = %d, want 300", m.BytesTx.Load())
	}
	if m.BytesRx.Load() != 50 {
		t.Errorf("BytesRx = %d, want 50", m.BytesRx.Load())
	}
	if m.Streams.Load() != 2 {
		t.Errorf("Streams = %d, want 2", m.Streams.Load())
	}
	if m.StartTime.Load() != 12345 {
		t.Errorf("StartTime = %d, want 12345", m.StartTime.Load())
	}
}

func TestSnapshotMetrics(t *testing.T) {
	t.Parallel()
	s := newState()
	m := s.GetMetrics("connection", "c1")
	m.BytesTx.Store(999)

	snap := s.SnapshotMetrics()
	if snap["connection:c1"] == nil {
		t.Fatal("SnapshotMetrics missing entry")
	}
	if snap["connection:c1"].BytesTx.Load() != 999 {
		t.Errorf("BytesTx = %d, want 999", snap["connection:c1"].BytesTx.Load())
	}
}

func TestSnapshotMetrics_Empty(t *testing.T) {
	t.Parallel()
	s := newState()
	snap := s.SnapshotMetrics()
	if len(snap) != 0 {
		t.Errorf("empty metrics snapshot has %d entries", len(snap))
	}
}

func TestDeleteMetrics(t *testing.T) {
	t.Parallel()
	s := newState()
	s.GetMetrics("connection", "c1")
	s.DeleteMetrics("connection", "c1")

	snap := s.SnapshotMetrics()
	if _, ok := snap["connection:c1"]; ok {
		t.Error("metrics still present after DeleteMetrics")
	}
}

func TestMetrics_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := newState()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := s.GetMetrics("connection", "shared")
			m.BytesTx.Add(int64(i))
			m.BytesRx.Add(int64(i))
			m.Streams.Add(1)
			m.Streams.Add(-1)
			s.SnapshotMetrics()
		}(i)
	}
	wg.Wait()
}

func TestSizes_Empty(t *testing.T) {
	t.Parallel()
	s := newState()
	comps, mets := s.Sizes()
	if comps != 0 {
		t.Errorf("components = %d, want 0", comps)
	}
	if mets != 0 {
		t.Errorf("metrics = %d, want 0", mets)
	}
}

func TestSizes_WithData(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "p1", Listening, "")
	s.Update("connection", "c1", Connected, "")
	s.GetMetrics("connection", "c1")

	comps, mets := s.Sizes()
	if comps != 2 {
		t.Errorf("components = %d, want 2", comps)
	}
	if mets != 1 {
		t.Errorf("metrics = %d, want 1", mets)
	}
}

func TestSizes_AfterDelete(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "p1", Listening, "")
	s.GetMetrics("proxy", "p1")

	s.Delete("proxy", "p1")
	s.DeleteMetrics("proxy", "p1")

	comps, mets := s.Sizes()
	if comps != 0 {
		t.Errorf("components = %d, want 0", comps)
	}
	if mets != 0 {
		t.Errorf("metrics = %d, want 0", mets)
	}
}

func TestEvictStale_RemovesOldTransientEntries(t *testing.T) {
	t.Parallel()
	s := newState()
	// Use a transient state (Retrying) — these are subject to eviction.
	s.Update("connection", "old", Retrying, "error")
	s.Update("connection", "fresh", Retrying, "error")

	// Manually backdate the "old" component.
	s.mu.Lock()
	comp := s.components["connection:old"]
	comp.LastUpdated = time.Now().Add(-2 * componentTTL)
	s.components["connection:old"] = comp
	s.mu.Unlock()

	// Also create metrics for both.
	s.GetMetrics("connection", "old")
	s.GetMetrics("connection", "fresh")

	s.evictStale(time.Now())

	snap := s.Snapshot()
	if _, ok := snap["connection:old"]; ok {
		t.Error("stale transient component should have been evicted")
	}
	if _, ok := snap["connection:fresh"]; !ok {
		t.Error("fresh component should remain")
	}

	mSnap := s.SnapshotMetrics()
	if _, ok := mSnap["connection:old"]; ok {
		t.Error("metrics for stale component should have been evicted")
	}
	if _, ok := mSnap["connection:fresh"]; !ok {
		t.Error("metrics for fresh component should remain")
	}
}

func TestEvictStale_SkipsStableStates(t *testing.T) {
	t.Parallel()
	s := newState()
	// Listening and Connected are stable — should never be evicted.
	s.Update("server", "listener", Listening, "")
	s.Update("connection", "conn", Connected, "target")

	// Backdate both beyond TTL.
	s.mu.Lock()
	for _, key := range []string{"server:listener", "connection:conn"} {
		comp := s.components[key]
		comp.LastUpdated = time.Now().Add(-2 * componentTTL)
		s.components[key] = comp
	}
	s.mu.Unlock()

	s.evictStale(time.Now())

	snap := s.Snapshot()
	if _, ok := snap["server:listener"]; !ok {
		t.Error("Listening component should NOT be evicted regardless of age")
	}
	if _, ok := snap["connection:conn"]; !ok {
		t.Error("Connected component should NOT be evicted regardless of age")
	}
}

func TestEvictStale_SkipsZeroLastUpdated(t *testing.T) {
	t.Parallel()
	s := newState()
	// Directly inject a component with zero LastUpdated (legacy).
	s.mu.Lock()
	s.components["proxy:legacy"] = Component{Type: "proxy", ID: "legacy", Status: Listening}
	s.mu.Unlock()

	s.evictStale(time.Now())

	snap := s.Snapshot()
	if _, ok := snap["proxy:legacy"]; !ok {
		t.Error("component with zero LastUpdated should not be evicted")
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := newState()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			s.Update("proxy", id, Listening, "")
			s.UpdateBind("proxy", id, "127.0.0.1:8080")
			s.Snapshot()
			s.Delete("proxy", id)
		}(i)
	}
	wg.Wait()
}

func BenchmarkSnapshot(b *testing.B) {
	s := &State{components: make(map[string]Component)}
	for i := range 100 {
		id := fmt.Sprintf("comp-%d", i)
		s.Update("server", id, Connected, "peer")
	}
	b.ResetTimer()
	for b.Loop() {
		_ = s.Snapshot()
	}
}

func BenchmarkSnapshotFull(b *testing.B) {
	s := &State{components: make(map[string]Component)}
	for i := range 100 {
		id := fmt.Sprintf("comp-%d", i)
		s.Update("server", id, Connected, "peer")
		m := s.GetMetrics("server", id)
		m.BytesTx.Store(int64(i * 1000))
	}
	b.ResetTimer()
	for b.Loop() {
		_ = s.SnapshotFull()
	}
}

func BenchmarkUpdateDelete(b *testing.B) {
	s := &State{components: make(map[string]Component)}
	b.ResetTimer()
	for b.Loop() {
		s.Update("bench", "id", Connecting, "")
		s.Update("bench", "id", Connected, "target")
		s.Delete("bench", "id")
	}
}
