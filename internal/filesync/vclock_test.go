package filesync

import (
	"strings"
	"testing"

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
