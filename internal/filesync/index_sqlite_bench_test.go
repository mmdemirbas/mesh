package filesync

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkLoadIndex_168kFiles measures the cost of loading a folder
// index of representative production size (168 000 file entries) from a
// SQLite database via loadIndexDB. The result gates the β/hybrid
// architecture choice in §6 commit 7 of PERSISTENCE-AUDIT.md:
//
//   - < 80 ms median → β proceeds (in-memory FileIndex discarded
//     between scans; SQLite SELECT runs at every scan start).
//   - ≥ 80 ms median → hybrid pivot (in-memory FileIndex retained
//     between scans; SELECT runs only at folder open).
//   - 75–85 ms borderline → default to hybrid. The cost asymmetry
//     (chronic per-scan regression vs. one extra in-memory map)
//     favors the conservative branch.
//
// Run with `go test -run NONE -bench BenchmarkLoadIndex_168kFiles
// -benchtime=1x -count=10 ./internal/filesync/`. The runner uses
// `-benchtime=1x` so each iteration is a single full load (the
// scan-start cost we are gating); `-count=10` produces ten samples
// for median + std-dev. Record the result in the commit message and
// in `RESEARCH.md`.
//
// The synthetic index uses 168 000 paths shaped like a realistic
// monorepo (varying depth and file-name length, mixed Unicode-free
// ASCII). Each row carries a non-empty VectorClock with two device
// entries and a 32-byte SHA-256 — the same schema shape the
// production scanner emits.
func BenchmarkLoadIndex_168kFiles(b *testing.B) {
	const folderID = "bench-folder"
	const n = 168_000

	dir := b.TempDir()
	db, err := openFolderDB(dir, "BENCHDEV01")
	if err != nil {
		b.Fatalf("openFolderDB: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	// Build the index once outside the timed loop.
	idx := newFileIndex()
	idx.Sequence = int64(n)
	idx.Epoch = "deadbeefcafef00d"
	idx.DeviceID = 0x0102030405060708
	for i := 0; i < n; i++ {
		path := syntheticPath(i)
		idx.Set(path, FileEntry{
			Size:     int64(i * 137),
			MtimeNS:  int64(1_700_000_000_000_000_000) + int64(i)*1_000_000,
			SHA256:   hash256FromBytes(syntheticHash(i)),
			Deleted:  i%173 == 0, // sparse tombstones, like real folders
			Sequence: int64(i + 1),
			Mode:     0o644,
			Inode:    uint64(1_000_000 + i),
			Version: VectorClock{
				"BENCHDEV01": uint64(i/7 + 1),
				"PEER000002": uint64(i/11 + 1),
			},
		})
	}
	idx.recomputeCache()

	if err := saveIndex(context.Background(), db, folderID, idx); err != nil {
		b.Fatalf("saveIndex: %v", err)
	}

	// Confirm the bench is measuring what we think — the row count
	// must match before we time the load.
	loaded, err := loadIndexDB(db, folderID)
	if err != nil {
		b.Fatalf("warm-up load: %v", err)
	}
	if got := loaded.Len(); got != n {
		b.Fatalf("warm-up load returned %d rows, want %d", got, n)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		got, err := loadIndexDB(db, folderID)
		if err != nil {
			b.Fatalf("loadIndexDB: %v", err)
		}
		if got.Len() != n {
			b.Fatalf("loaded %d rows, want %d", got.Len(), n)
		}
	}
}

// syntheticPath produces a realistic-shape repository path for index i.
// The shape mixes shallow and deep paths with varying segment lengths
// so the SQLite primary-key compare cost approximates real-monorepo
// load. Deterministic — same i always yields the same path.
func syntheticPath(i int) string {
	depth := 1 + i%6
	parts := make([]string, 0, depth+1)
	for d := 0; d < depth; d++ {
		// Each segment 4–12 chars, base-36 encoded.
		seg := fmt.Sprintf("d%x_%d", (i*31+d*7)&0xfff, d)
		parts = append(parts, seg)
	}
	parts = append(parts, fmt.Sprintf("file_%06d.dat", i))
	out := parts[0]
	for k := 1; k < len(parts); k++ {
		out += "/" + parts[k]
	}
	return out
}

// syntheticHash returns a 32-byte hash that varies with i. We avoid
// hashing real bytes — the bench measures load cost, not crypto.
func syntheticHash(i int) []byte {
	out := make([]byte, 32)
	for k := 0; k < 32; k++ {
		out[k] = byte((i + k*13) & 0xff)
	}
	return out
}

// BenchmarkConcurrentReaderDuringScan measures peer-facing read
// latency through the dedicated read-only handle while the writer
// pool (MaxOpenConns=1) is busy applying a saveIndex transaction in
// the background. The audit's INV-1 invariant says peer exchanges
// must NOT serialize behind the scanner — WAL snapshot isolation +
// the second sql.DB handle is the mechanism. This benchmark is the
// instrument that proves it.
//
// What we measure: median wall time of a single
// queryFilesSinceSeq(sinceSeq=N/2) call (yields ~half the rows)
// while a background goroutine loops saveIndex transactions on a
// disjoint dirty subset. Reported as ns/op on the reader call.
//
// Acceptance: not a CI gate — this is a measurement that informs the
// β/hybrid follow-up in §6 commit 7. We record the number in the
// commit message; a regression of >2× across releases is an audit
// re-open trigger.
//
// Run with `go test -run NONE -bench BenchmarkConcurrentReaderDuringScan
// -benchtime=2s -count=5 ./internal/filesync/` (single connection
// per direction, real fsync; the run takes a few seconds).
func BenchmarkConcurrentReaderDuringScan(b *testing.B) {
	const folderID = "bench-folder"
	const n = 50_000

	dir := b.TempDir()
	writer, err := openFolderDB(dir, "BENCHDEV01")
	if err != nil {
		b.Fatalf("openFolderDB: %v", err)
	}
	b.Cleanup(func() { _ = writer.Close() })

	// Seed a baseline so queryFilesSinceSeq has rows to walk.
	idx := newFileIndex()
	idx.Sequence = int64(n)
	idx.Epoch = "deadbeefcafef00d"
	idx.DeviceID = 0x0102030405060708
	for i := 0; i < n; i++ {
		idx.Set(syntheticPath(i), FileEntry{
			Size:     int64(i),
			MtimeNS:  int64(1_700_000_000_000_000_000) + int64(i)*1_000_000,
			SHA256:   hash256FromBytes(syntheticHash(i)),
			Sequence: int64(i + 1),
			Mode:     0o644,
			Inode:    uint64(1_000_000 + i),
			Version:  VectorClock{"BENCHDEV01": uint64(i/7 + 1)},
		})
	}
	idx.recomputeCache()
	if err := saveIndex(context.Background(), writer, folderID, idx); err != nil {
		b.Fatalf("seed saveIndex: %v", err)
	}

	reader, err := openFolderDBReader(dir, 4)
	if err != nil {
		b.Fatalf("openFolderDBReader: %v", err)
	}
	b.Cleanup(func() { _ = reader.Close() })

	// Background writer: rewrite a small dirty subset on each tick so
	// the writer connection is held for a measurable fraction of the
	// run. Stops when the bench loop signals.
	stop := make(chan struct{})
	var writes atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		dirty := newFileIndex()
		dirty.Sequence = int64(n)
		dirty.Epoch = idx.Epoch
		dirty.DeviceID = idx.DeviceID
		bumpSeq := int64(n) + 1
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Flip 64 paths per tx — small enough to keep tx duration
			// bounded, large enough that the writer is holding the
			// connection across multiple reader iterations.
			for k := 0; k < 64; k++ {
				p := syntheticPath((int(writes.Load())*64 + k) % n)
				e, _ := dirty.Get(p)
				e.Size++
				e.MtimeNS = time.Now().UnixNano()
				e.Sequence = bumpSeq
				bumpSeq++
				dirty.Set(p, e)
			}
			dirty.Sequence = bumpSeq - 1
			if err := saveIndex(context.Background(), writer, folderID, dirty); err != nil {
				b.Errorf("background saveIndex: %v", err)
				return
			}
			writes.Add(1)
			dirty.ClearDirty()
		}
	}()
	b.Cleanup(func() {
		close(stop)
		<-done
	})

	since := int64(n / 2)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var rows int
		err := queryFilesSinceSeq(context.Background(), reader, folderID, since,
			func(_ string, _ FileEntry) bool {
				rows++
				return true
			})
		if err != nil {
			b.Fatalf("queryFilesSinceSeq: %v", err)
		}
		if rows == 0 {
			b.Fatalf("queryFilesSinceSeq returned no rows")
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(writes.Load()), "writer_txs")
}
