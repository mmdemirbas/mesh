package filesync

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"testing"
)

// Deterministic pseudo-random data so every run produces identical input
// and the chunker's determinism can be tested byte-for-byte.
func mkData(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test fixture
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

func chunkAll(t *testing.T, data []byte) []Block {
	t.Helper()
	blocks, err := ChunkFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}
	return blocks
}

// TestFastCDC_Deterministic ensures two runs over the same bytes produce
// identical chunks — a cross-peer contract.
func TestFastCDC_Deterministic(t *testing.T) {
	data := mkData(1, 4*1024*1024)
	a := chunkAll(t, data)
	b := chunkAll(t, data)
	if len(a) != len(b) {
		t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chunk %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestFastCDC_CoversInput — offsets and lengths must tile the input with
// no gaps or overlaps, and hashes must match the actual bytes.
func TestFastCDC_CoversInput(t *testing.T) {
	data := mkData(2, 3*1024*1024+12345)
	blocks := chunkAll(t, data)
	if len(blocks) == 0 {
		t.Fatalf("no blocks for non-empty input")
	}
	var want int64
	for i, b := range blocks {
		if b.Offset != want {
			t.Fatalf("block %d offset=%d want %d", i, b.Offset, want)
		}
		if b.Length <= 0 {
			t.Fatalf("block %d non-positive length %d", i, b.Length)
		}
		h := sha256.Sum256(data[b.Offset : b.Offset+int64(b.Length)])
		if h != b.Hash {
			t.Fatalf("block %d hash mismatch", i)
		}
		want += int64(b.Length)
	}
	if want != int64(len(data)) {
		t.Fatalf("blocks cover %d bytes, want %d", want, len(data))
	}
}

// TestFastCDC_SizeBounds — non-final blocks stay within [min, max]; the
// final block may be smaller than min if the file is short.
func TestFastCDC_SizeBounds(t *testing.T) {
	data := mkData(3, 5*1024*1024)
	blocks := chunkAll(t, data)
	for i, b := range blocks {
		if b.Length > fastCDCMax {
			t.Fatalf("block %d length %d exceeds max %d", i, b.Length, fastCDCMax)
		}
		if i < len(blocks)-1 && b.Length < fastCDCMin {
			t.Fatalf("non-final block %d length %d below min %d", i, b.Length, fastCDCMin)
		}
	}
}

// TestFastCDC_Empty — empty reader produces no blocks and returns cleanly.
func TestFastCDC_Empty(t *testing.T) {
	blocks := chunkAll(t, nil)
	if len(blocks) != 0 {
		t.Fatalf("empty input produced %d blocks", len(blocks))
	}
}

// TestFastCDC_ShorterThanMin — input shorter than min emits a single
// block at EOF covering everything.
func TestFastCDC_ShorterThanMin(t *testing.T) {
	data := mkData(4, fastCDCMin/2)
	blocks := chunkAll(t, data)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Length != len(data) {
		t.Fatalf("length=%d want %d", blocks[0].Length, len(data))
	}
}

// TestFastCDC_ContentDefined — inserting bytes near the start shifts
// only the first few chunks; later chunks re-synchronize. This is the
// whole point of content-defined chunking vs fixed-size blocks.
func TestFastCDC_ContentDefined(t *testing.T) {
	base := mkData(5, 2*1024*1024)
	// Insert 64 bytes of zeros near the start.
	var mutated []byte
	mutated = append(mutated, base[:1024]...)
	mutated = append(mutated, make([]byte, 64)...)
	mutated = append(mutated, base[1024:]...)

	aBlocks := chunkAll(t, base)
	bBlocks := chunkAll(t, mutated)

	hashA := map[Hash256]struct{}{}
	for _, b := range aBlocks {
		hashA[b.Hash] = struct{}{}
	}
	shared := 0
	for _, b := range bBlocks {
		if _, ok := hashA[b.Hash]; ok {
			shared++
		}
	}
	// With 64 inserted bytes in a 2 MiB stream, most chunks past the
	// edit region should re-sync and re-appear in the mutated output.
	// Allow a generous floor; the exact number depends on the gear
	// table. Require at least a third to be shared.
	minShared := len(aBlocks) / 3
	if shared < minShared {
		t.Fatalf("content-defined re-sync too weak: shared=%d of %d (min %d)",
			shared, len(aBlocks), minShared)
	}
}

// TestFastCDC_StreamingMatchesChunkFile — driving Next() directly
// yields the same chunks as the ChunkFile helper.
func TestFastCDC_StreamingMatchesChunkFile(t *testing.T) {
	data := mkData(6, 1500000)
	want := chunkAll(t, data)

	c := newDefaultChunker(bytes.NewReader(data))
	var got []Block
	for {
		ch, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		h := sha256.Sum256(ch.Data)
		got = append(got, Block{Offset: ch.Offset, Length: ch.Length, Hash: h})
	}
	if len(got) != len(want) {
		t.Fatalf("count mismatch: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("block %d differs", i)
		}
	}
}

// TestFastCDC_InvalidParams — constructor rejects bad parameters.
func TestFastCDC_InvalidParams(t *testing.T) {
	cases := []struct {
		name          string
		min, avg, max int
	}{
		{"zero min", 0, 128, 512},
		{"negative min", -1, 128, 512},
		{"avg not greater than min", 128, 128, 512},
		{"max not greater than avg", 128, 256, 256},
		{"inverted", 512, 256, 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewChunker(bytes.NewReader(nil), tc.min, tc.avg, tc.max); err == nil {
				t.Fatalf("expected error for %+v", tc)
			}
		})
	}
}

// TestFastCDC_GearTableDeterministic — the derived gear table must not
// drift between runs; any change invalidates every persisted index.
func TestFastCDC_GearTableDeterministic(t *testing.T) {
	a := buildGearTable()
	b := buildGearTable()
	if a != b {
		t.Fatalf("gear table not deterministic")
	}
	// Spot-check a couple of entries so an accidental init-order change
	// in buildGearTable triggers a visible failure rather than a silent
	// boundary drift.
	if a[0] == 0 || a[255] == 0 {
		t.Fatalf("gear table has zero entries at 0 or 255")
	}
}
