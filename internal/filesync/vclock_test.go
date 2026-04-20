package filesync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"gopkg.in/yaml.v3"
)

func TestCompareClocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b VectorClock
		want ClockOrder
	}{
		{"both-empty", nil, nil, ClockEqual},
		{"empty-vs-nonempty", nil, VectorClock{"A": 1}, ClockBefore},
		{"nonempty-vs-empty", VectorClock{"A": 1}, nil, ClockAfter},
		{"identical", VectorClock{"A": 3, "B": 1}, VectorClock{"A": 3, "B": 1}, ClockEqual},
		{"a-dominates-by-bump", VectorClock{"A": 3, "B": 1}, VectorClock{"A": 2, "B": 1}, ClockAfter},
		{"b-dominates-by-bump", VectorClock{"A": 2, "B": 1}, VectorClock{"A": 3, "B": 1}, ClockBefore},
		{"a-dominates-by-new-device", VectorClock{"A": 1, "C": 1}, VectorClock{"A": 1}, ClockAfter},
		{"b-dominates-by-new-device", VectorClock{"A": 1}, VectorClock{"A": 1, "C": 1}, ClockBefore},
		{"concurrent-split", VectorClock{"A": 2, "B": 1}, VectorClock{"A": 1, "B": 2}, ClockConcurrent},
		{"concurrent-disjoint-devices", VectorClock{"A": 1}, VectorClock{"B": 1}, ClockConcurrent},
		{"absent-vs-zero-equal", VectorClock{"A": 1}, VectorClock{"A": 1, "B": 0}, ClockEqual},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareClocks(tc.a, tc.b); got != tc.want {
				t.Fatalf("compareClocks(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestVectorClock_Bump(t *testing.T) {
	t.Parallel()

	// First bump starts at 1 even on a nil clock.
	got := VectorClock(nil).bump("ABCDE-12345")
	if got["ABCDE-12345"] != 1 {
		t.Fatalf("first bump: got %d, want 1", got["ABCDE-12345"])
	}

	// bump is immutable: the receiver must not be modified.
	base := VectorClock{"A": 3, "B": 2}
	next := base.bump("A")
	if base["A"] != 3 {
		t.Fatalf("bump mutated receiver: base[A] = %d", base["A"])
	}
	if next["A"] != 4 {
		t.Fatalf("next[A] = %d, want 4", next["A"])
	}
	if next["B"] != 2 {
		t.Fatalf("next[B] = %d, want 2 (unrelated counters must carry over)", next["B"])
	}

	// Bumping a new device appears as a new entry at 1.
	third := next.bump("C")
	if third["C"] != 1 {
		t.Fatalf("new device: third[C] = %d, want 1", third["C"])
	}
}

func TestVectorClock_ProtoRoundTrip(t *testing.T) {
	t.Parallel()

	// Nil ⇄ nil.
	if got := VectorClock(nil).toProto(); got != nil {
		t.Fatalf("nil.toProto() = %v, want nil", got)
	}
	if got := vectorClockFromProto(nil); got != nil {
		t.Fatalf("vectorClockFromProto(nil) = %v, want nil", got)
	}

	// Non-empty round-trip preserves values and drops zero entries.
	src := VectorClock{"A": 1, "B": 2, "C": 0}
	wire := src.toProto()
	if len(wire) != 2 {
		t.Fatalf("toProto dropped zero entry: len=%d, want 2", len(wire))
	}
	// Wire form is sorted for determinism.
	if wire[0].GetDeviceId() != "A" || wire[1].GetDeviceId() != "B" {
		t.Fatalf("toProto not sorted: %q then %q", wire[0].GetDeviceId(), wire[1].GetDeviceId())
	}
	got := vectorClockFromProto(wire)
	if len(got) != 2 || got["A"] != 1 || got["B"] != 2 {
		t.Fatalf("round-trip: got %v, want {A:1 B:2}", got)
	}
	if _, ok := got["C"]; ok {
		t.Fatalf("round-trip kept zero entry: got %v", got)
	}
}

func TestVectorClockFromProto_DedupsAndIgnoresGarbage(t *testing.T) {
	t.Parallel()

	// Duplicate device_id: keep the highest.
	counters := []*pb.Counter{
		{DeviceId: "A", Value: 1},
		{DeviceId: "A", Value: 5},
		{DeviceId: "B", Value: 2},
		{DeviceId: "A", Value: 3}, // not the max
		{DeviceId: "", Value: 99}, // empty id — drop
		nil,                       // nil entry — drop
	}
	got := vectorClockFromProto(counters)
	if got["A"] != 5 {
		t.Fatalf("dedup: got[A] = %d, want 5", got["A"])
	}
	if got["B"] != 2 {
		t.Fatalf("dedup: got[B] = %d, want 2", got["B"])
	}
	if _, ok := got[""]; ok {
		t.Fatal("empty device_id was not dropped")
	}
}

func TestFileEntry_VersionYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	entry := FileEntry{
		Size:    100,
		MtimeNS: 42,
		SHA256:  testHash("hello"),
		Version: VectorClock{"ABCDE-12345": 2, "FGHJK-67890": 1},
	}

	data, err := yaml.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got FileEntry
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version["ABCDE-12345"] != 2 || got.Version["FGHJK-67890"] != 1 {
		t.Fatalf("round-trip lost entries: got %v", got.Version)
	}

	// Empty clock must not land in YAML as a "version:" key.
	bare := FileEntry{Size: 1, SHA256: testHash("x")}
	data2, err := yaml.Marshal(bare)
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(data2), "version:") {
		t.Fatalf("bare FileEntry emitted version key: %s", data2)
	}
}

func TestFileInfo_VersionWireRoundTrip(t *testing.T) {
	t.Parallel()

	entry := FileEntry{
		Size:    100,
		MtimeNS: 42,
		SHA256:  testHash("abc"),
		Version: VectorClock{"ABCDE-12345": 3, "ZZZZZ-99999": 1},
	}

	// Simulate the wire path: FileEntry → FileInfo → back via protoToFileIndex.
	idx := &pb.IndexExchange{
		ProtocolVersion: protocolVersion,
		FolderId:        "f",
		Files: []*pb.FileInfo{{
			Path:    "p.txt",
			Size:    entry.Size,
			MtimeNs: entry.MtimeNS,
			Sha256:  entry.SHA256[:],
			Version: entry.Version.toProto(),
		}},
	}

	got := protoToFileIndex(idx)
	back, ok := got.Files["p.txt"]
	if !ok {
		t.Fatal("entry missing after round-trip")
	}
	if compareClocks(back.Version, entry.Version) != ClockEqual {
		t.Fatalf("wire round-trip lost clock: got %v, want %v", back.Version, entry.Version)
	}
}

// TestScan_BumpsSelfOnLocalWrite pins the C6 invariant that every local
// write (new file, content change, deletion) increments the self counter
// in the FileEntry's vector clock. Without this, peers can't distinguish
// "we wrote it" from "they wrote it" and concurrency detection collapses.
func TestScan_BumpsSelfOnLocalWrite(t *testing.T) {
	t.Parallel()

	const selfID = "AAAAA-11111"
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "one")

	idx := newFileIndex()
	idx.selfID = selfID

	// First write: new entry → Version{self: 1}.
	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	e1, ok := idx.Files["a.txt"]
	if !ok {
		t.Fatal("a.txt missing after first scan")
	}
	if e1.Version[selfID] != 1 {
		t.Fatalf("first write: Version[self]=%d, want 1 (full=%v)", e1.Version[selfID], e1.Version)
	}

	// Content change: bump to self: 2.
	writeFile(t, dir, "a.txt", "two-different-content")
	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	e2 := idx.Files["a.txt"]
	if e2.Version[selfID] != 2 {
		t.Fatalf("content change: Version[self]=%d, want 2 (full=%v)", e2.Version[selfID], e2.Version)
	}
	if compareClocks(e2.Version, e1.Version) != ClockAfter {
		t.Fatalf("post-change clock must dominate pre-change: got %v, want ClockAfter", compareClocks(e2.Version, e1.Version))
	}

	// Local delete: tombstone bumps self to 3.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	e3 := idx.Files["a.txt"]
	if !e3.Deleted {
		t.Fatal("a.txt not tombstoned after local delete")
	}
	if e3.Version[selfID] != 3 {
		t.Fatalf("tombstone: Version[self]=%d, want 3 (full=%v)", e3.Version[selfID], e3.Version)
	}
}

// TestScan_StatOnlyChangeDoesNotBump pins that a stat-only change (mtime
// bump with identical content) does NOT increment the vector clock.
// Version is content-semantic, not stat-semantic.
func TestScan_StatOnlyChangeDoesNotBump(t *testing.T) {
	t.Parallel()

	const selfID = "BBBBB-22222"
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "stable")

	idx := newFileIndex()
	idx.selfID = selfID

	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	before := idx.Files["a.txt"].Version.clone()

	// Touch mtime without changing content — forces the hash path but
	// the same-hash branch must NOT bump.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	after := idx.Files["a.txt"].Version
	if compareClocks(before, after) != ClockEqual {
		t.Fatalf("stat-only change bumped clock: before=%v after=%v", before, after)
	}
}

// TestScan_EmptySelfIDSkipsBump pins the tests-and-migration guard:
// FileIndex built without a selfID (test harness, pre-C6 persisted
// indexes before rehydration) must not populate Version with an empty
// device ID entry.
func TestScan_EmptySelfIDSkipsBump(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x")

	idx := newFileIndex() // selfID left empty
	if _, _, _, err := idx.scan(context.Background(), dir, newIgnoreMatcher(nil)); err != nil {
		t.Fatal(err)
	}
	e := idx.Files["a.txt"]
	if len(e.Version) != 0 {
		t.Fatalf("empty selfID leaked into Version: %v", e.Version)
	}
}

// TestDiff_PopulatesRemoteVersion pins that diff() carries the peer's
// vector clock forward on every action type. Without this, receive-side
// handlers have no way to adopt the remote clock and each sync would
// leave stale Version maps in the local index.
func TestDiff_PopulatesRemoteVersion(t *testing.T) {
	t.Parallel()

	// Remote has: p_new.txt (new), p_mod.txt (content changed), p_del.txt (tombstoned).
	// Local has: p_mod.txt (older content), p_del.txt (unchanged).
	local := &FileIndex{Files: map[string]FileEntry{
		"p_mod.txt": {Size: 1, MtimeNS: 1, SHA256: testHash("local-old")},
		"p_del.txt": {Size: 1, MtimeNS: 1, SHA256: testHash("stable")},
	}}
	remote := &FileIndex{Files: map[string]FileEntry{
		"p_new.txt": {
			Size: 1, MtimeNS: 10, SHA256: testHash("new"), Sequence: 1,
			Version: VectorClock{"PEER-AA": 1},
		},
		"p_mod.txt": {
			Size: 1, MtimeNS: 10, SHA256: testHash("remote-new"), Sequence: 2,
			Version: VectorClock{"PEER-AA": 2},
		},
		"p_del.txt": {
			Deleted: true, Size: 1, MtimeNS: 10, Sequence: 3,
			Version: VectorClock{"PEER-AA": 3},
		},
	}}
	// lastSeenSeq=0 so remote entries are all new; lastSyncNS=100 (>local mtimes)
	// so C1 mtime fallback classifies p_mod.txt as Download (local unchanged).
	// Also set lastSeenSeq to allow tombstone delivery (lastSeenSeq > 0 branch).
	actions := local.diff(remote, 0, 100, nil, "send-receive")

	byPath := map[string]DiffEntry{}
	for _, a := range actions {
		byPath[a.Path] = a
	}

	if got := byPath["p_new.txt"].RemoteVersion["PEER-AA"]; got != 1 {
		t.Errorf("p_new.txt RemoteVersion[PEER-AA]=%d, want 1", got)
	}
	if got := byPath["p_mod.txt"].RemoteVersion["PEER-AA"]; got != 2 {
		t.Errorf("p_mod.txt RemoteVersion[PEER-AA]=%d, want 2 (action=%v)",
			got, byPath["p_mod.txt"].Action)
	}
	// Tombstones are only emitted when lastSeenSeq > 0, so rerun with a baseline.
	actions2 := local.diff(remote, 0, 100, nil, "send-receive") // p_del.txt suppressed
	if _, ok := actionPathMap(actions2)["p_del.txt"]; ok {
		t.Fatal("unexpected: tombstone emitted on first sync")
	}
	actions3 := local.diff(remote, 0, 100, map[string]Hash256{
		"p_del.txt": testHash("stable"),
	}, "send-receive")
	// Still no delete because H8 first-sync guard is gated on lastSeenSeq=0.
	if _, ok := actionPathMap(actions3)["p_del.txt"]; ok {
		t.Fatal("first-sync guard broken")
	}
	// Now with a baseline (lastSeenSeq > 0) the tombstone is emitted.
	actions4 := local.diff(remote, 0 /*lastSeen*/, 100, nil, "send-receive")
	_ = actions4
	withBaseline := local.diff(remote, 2, 100, nil, "send-receive")
	del, ok := actionPathMap(withBaseline)["p_del.txt"]
	if !ok {
		t.Fatal("tombstone not emitted with lastSeenSeq=2")
	}
	if del.Action != ActionDelete {
		t.Fatalf("p_del.txt action=%v, want ActionDelete", del.Action)
	}
	if got := del.RemoteVersion["PEER-AA"]; got != 3 {
		t.Errorf("p_del.txt RemoteVersion[PEER-AA]=%d, want 3", got)
	}
}

func actionPathMap(actions []DiffEntry) map[string]DiffEntry {
	out := make(map[string]DiffEntry, len(actions))
	for _, a := range actions {
		out[a.Path] = a
	}
	return out
}

// TestFileIndex_CloneInto_DeepCopiesVersion pins that scan clones do not
// alias Version maps — mutating the clone must not reach into the
// source. Without this, the scan's private copy would corrupt the live
// index observed by syncs and the admin UI.
func TestFileIndex_CloneInto_DeepCopiesVersion(t *testing.T) {
	t.Parallel()

	src := newFileIndex()
	src.Files["a.txt"] = FileEntry{
		Size:    1,
		SHA256:  testHash("x"),
		Version: VectorClock{"A": 1},
	}

	cp := src.clone()
	entry := cp.Files["a.txt"]
	entry.Version["A"] = 999
	cp.Files["a.txt"] = entry

	if src.Files["a.txt"].Version["A"] != 1 {
		t.Fatalf("clone aliases Version: src[A]=%d after mutating clone",
			src.Files["a.txt"].Version["A"])
	}
}

func TestVectorClock_Clone(t *testing.T) {
	t.Parallel()

	if VectorClock(nil).clone() != nil {
		t.Fatal("nil.clone() must stay nil")
	}

	src := VectorClock{"A": 1}
	cp := src.clone()
	cp["A"] = 99
	if src["A"] != 1 {
		t.Fatalf("clone aliases receiver: src[A] = %d after mutating clone", src["A"])
	}
}
