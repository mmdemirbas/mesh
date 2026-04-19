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

// TestEvictStale_RealWorldScenario reproduces the production bug where a running
// mesh instance lost all dashboard status after ~1 hour. The root cause was that
// long-lived components (SSH server listener, SSH client connections, port
// forwards, proxies, clipsync, filesync) set their state to Listening/Connected
// once at startup and never called Update again. After componentTTL (1 hour),
// evictStale removed them, causing all status indicators to turn white/gray.
//
// This test sets up every component type that exists in a real deployment,
// advances time beyond the TTL, and verifies none of them are evicted.
func TestEvictStale_RealWorldScenario(t *testing.T) {
	t.Parallel()
	s := newState()

	// Simulate a real mesh node with all component types.
	components := []struct {
		compType string
		id       string
		status   Status
	}{
		// SSH server listening on port 2222
		{"server", "0.0.0.0:2222", Listening},
		// SOCKS and HTTP proxy listeners
		{"proxy", "127.0.0.1:2080", Listening},
		{"proxy", "127.0.0.1:2081", Listening},
		// Outbound SSH connection to bastion
		{"connection", "bastion [tunnel-sshd]", Connected},
		{"connection", "bastion [tunnel-proxy]", Connected},
		// Local port forwards
		{"forward", "bastion [tunnel-sshd] 127.0.0.1:8080", Listening},
		{"forward", "bastion [tunnel-proxy] 127.0.0.1:1080", Listening},
		// Remote forwards (registered by SSH clients connecting to our server)
		{"dynamic", "127.0.0.1:1111|2222", Listening},
		{"dynamic", "127.0.0.1:18384|2222", Listening},
		// Clipsync
		{"clipsync", "0.0.0.0:7755", Listening},
		// Filesync
		{"filesync", "0.0.0.0:7756", Listening},
		{"filesync-folder", "docs", Connected},
	}

	for _, c := range components {
		s.Update(c.compType, c.id, c.status, "")
		s.GetMetrics(c.compType, c.id)
	}

	// Simulate 2 hours passing without any state updates — this is the exact
	// scenario that caused the production bug. Long-lived components never
	// call Update after their initial state transition.
	s.mu.Lock()
	for key, comp := range s.components {
		comp.LastUpdated = time.Now().Add(-2 * componentTTL)
		s.components[key] = comp
	}
	s.mu.Unlock()

	s.evictStale(time.Now())

	snap := s.Snapshot()
	for _, c := range components {
		key := c.compType + ":" + c.id
		if _, ok := snap[key]; !ok {
			t.Errorf("component %s should survive eviction (status=%s)", key, c.status)
		}
	}

	// Verify metrics are also intact.
	mSnap := s.SnapshotMetrics()
	for _, c := range components {
		key := c.compType + ":" + c.id
		if _, ok := mSnap[key]; !ok {
			t.Errorf("metrics for %s should survive eviction", key)
		}
	}
}

// TestEvictStale_TransientStatesStillEvicted verifies that the stable-state
// exemption does not prevent cleanup of genuinely stale transient entries.
// Components stuck in Starting/Connecting/Retrying/Failed indicate crashed
// goroutines and must still be reaped.
func TestEvictStale_TransientStatesStillEvicted(t *testing.T) {
	t.Parallel()
	s := newState()

	transient := []struct {
		id     string
		status Status
	}{
		{"stuck-starting", Starting},
		{"stuck-connecting", Connecting},
		{"stuck-retrying", Retrying},
		{"stuck-failed", Failed},
	}

	for _, c := range transient {
		s.Update("connection", c.id, c.status, "error")
		s.GetMetrics("connection", c.id)
	}

	// Backdate beyond TTL.
	s.mu.Lock()
	for key, comp := range s.components {
		comp.LastUpdated = time.Now().Add(-2 * componentTTL)
		s.components[key] = comp
	}
	s.mu.Unlock()

	s.evictStale(time.Now())

	snap := s.Snapshot()
	for _, c := range transient {
		key := "connection:" + c.id
		if _, ok := snap[key]; ok {
			t.Errorf("transient component %s (status=%s) should be evicted after TTL", key, c.status)
		}
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

func TestMetrics_Reset(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	m.BytesTx.Store(100)
	m.BytesRx.Store(200)
	m.Streams.Store(5)
	m.TokensIn.Store(1000)
	m.TokensOut.Store(2000)
	m.TokensCacheRd.Store(300)
	m.TokensCacheWr.Store(400)
	m.TokensReason.Store(500)
	m.StartTime.Store(12345)

	before := time.Now().UnixNano()
	m.Reset()
	after := time.Now().UnixNano()

	if got := m.BytesTx.Load(); got != 0 {
		t.Errorf("BytesTx = %d, want 0", got)
	}
	if got := m.BytesRx.Load(); got != 0 {
		t.Errorf("BytesRx = %d, want 0", got)
	}
	if got := m.Streams.Load(); got != 0 {
		t.Errorf("Streams = %d, want 0", got)
	}
	if got := m.TokensIn.Load(); got != 0 {
		t.Errorf("TokensIn = %d, want 0", got)
	}
	if got := m.TokensOut.Load(); got != 0 {
		t.Errorf("TokensOut = %d, want 0", got)
	}
	if got := m.TokensCacheRd.Load(); got != 0 {
		t.Errorf("TokensCacheRd = %d, want 0", got)
	}
	if got := m.TokensCacheWr.Load(); got != 0 {
		t.Errorf("TokensCacheWr = %d, want 0", got)
	}
	if got := m.TokensReason.Load(); got != 0 {
		t.Errorf("TokensReason = %d, want 0", got)
	}
	if st := m.StartTime.Load(); st < before || st > after {
		t.Errorf("StartTime = %d, want in [%d, %d]", st, before, after)
	}
}

func TestUpdateFileCount(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("filesync", "folder1", Scanning, "")
	s.UpdateFileCount("filesync", "folder1", 42, 1_234_567)

	snap := s.Snapshot()
	comp := snap["filesync:folder1"]
	if comp.FileCount != 42 {
		t.Errorf("FileCount = %d, want 42", comp.FileCount)
	}
	if comp.TotalSize != 1_234_567 {
		t.Errorf("TotalSize = %d, want 1234567", comp.TotalSize)
	}
	if comp.Status != Scanning {
		t.Errorf("Status = %q (should be preserved), want %q", comp.Status, Scanning)
	}
}

func TestUpdateLastSync(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("filesync", "f1", Connected, "")
	ts := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	s.UpdateLastSync("filesync", "f1", ts)

	snap := s.Snapshot()
	if got := snap["filesync:f1"].LastSync; !got.Equal(ts) {
		t.Errorf("LastSync = %v, want %v", got, ts)
	}
}

func TestUpdateTLSFingerprint(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("filesync-peer", "bind|peer", Connected, "")
	const fp = "sha256:abc123def456"
	s.UpdateTLSFingerprint("filesync-peer", "bind|peer", fp)

	snap := s.Snapshot()
	if got := snap["filesync-peer:bind|peer"].TLSFingerprint; got != fp {
		t.Errorf("TLSFingerprint = %q, want %q", got, fp)
	}
}

func TestUpdateTLSStatus(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("filesync-peer", "bind|peer", Connected, "")
	s.UpdateTLSStatus("filesync-peer", "bind|peer", "encrypted · verified")

	snap := s.Snapshot()
	if got := snap["filesync-peer:bind|peer"].TLSStatus; got != "encrypted · verified" {
		t.Errorf("TLSStatus = %q, want %q", got, "encrypted · verified")
	}
}

func TestSnapshotFull(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "p1", Listening, "")
	s.UpdateBind("proxy", "p1", "127.0.0.1:1080")
	m := s.GetMetrics("proxy", "p1")
	m.BytesTx.Store(500)

	snap := s.SnapshotFull()

	if len(snap.Components) != 1 {
		t.Errorf("Components len = %d, want 1", len(snap.Components))
	}
	if got := snap.Components["proxy:p1"].BoundAddr; got != "127.0.0.1:1080" {
		t.Errorf("BoundAddr = %q, want 127.0.0.1:1080", got)
	}
	if len(snap.Metrics) != 1 {
		t.Errorf("Metrics len = %d, want 1", len(snap.Metrics))
	}
	if got := snap.Metrics["proxy:p1"].BytesTx.Load(); got != 500 {
		t.Errorf("BytesTx = %d, want 500", got)
	}
}

func TestSnapshotFull_Empty(t *testing.T) {
	t.Parallel()
	s := newState()
	snap := s.SnapshotFull()
	if len(snap.Components) != 0 {
		t.Errorf("Components non-empty in fresh state: %d", len(snap.Components))
	}
	if len(snap.Metrics) != 0 {
		t.Errorf("Metrics non-empty in fresh state: %d", len(snap.Metrics))
	}
}

func TestSnapshotFull_IsACopy(t *testing.T) {
	t.Parallel()
	s := newState()
	s.Update("proxy", "p1", Listening, "")

	snap := s.SnapshotFull()
	snap.Components["proxy:p1"] = Component{Message: "mutated"}

	snap2 := s.SnapshotFull()
	if snap2.Components["proxy:p1"].Message == "mutated" {
		t.Error("SnapshotFull returned a reference to internal map, not a copy")
	}
}
