package filesync

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// signFile runs FastCDC over path and returns the chunk signatures the
// receiver sends to the sender as BlockSignatures.blocks. See
// docs/filesync/DESIGN-v1.md §2.
func signFile(path string) ([]Block, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path validated by caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ChunkFile(f)
}

// signFileRoot is the os.Root-safe variant of signFile.
func signFileRoot(root *os.Root, relPath string) ([]Block, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ChunkFile(f)
}

// computeDelta chunks path with FastCDC and returns the sender's
// complete chunk list. Chunks whose hash appears in peerHashes are
// returned with empty Data — the receiver reassembles them from its
// own local copy. New chunks carry Data inline.
func computeDelta(path string, peerHashes map[Hash256]struct{}) ([]senderChunk, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path validated by caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readSenderChunks(f, peerHashes)
}

// computeDeltaRoot is the os.Root-safe variant of computeDelta.
func computeDeltaRoot(root *os.Root, relPath string, peerHashes map[Hash256]struct{}) ([]senderChunk, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readSenderChunks(f, peerHashes)
}

// senderChunk is the sender-side view of one FastCDC chunk. It maps
// 1:1 to the wire's DeltaBlock but keeps the bytes on the Go side so
// callers can decide whether to include data.
type senderChunk struct {
	Offset int64
	Length int
	Hash   Hash256
	Data   []byte // nil when peer already has Hash
}

func readSenderChunks(r io.Reader, peerHashes map[Hash256]struct{}) ([]senderChunk, error) {
	chunker := newDefaultChunker(r)
	var out []senderChunk
	for {
		ch, err := chunker.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fastcdc: %w", err)
		}
		h := sha256.Sum256(ch.Data)
		hash := hash256FromBytes(h[:])
		sc := senderChunk{Offset: ch.Offset, Length: ch.Length, Hash: hash}
		if _, ok := peerHashes[hash]; !ok {
			sc.Data = make([]byte, ch.Length)
			copy(sc.Data, ch.Data)
		}
		out = append(out, sc)
	}
}

// applyDeltaRoot reconstructs the remote file at relPath + ".mesh-delta-tmp-<peerID>"
// by walking chunks in offset order. Each chunk either carries inline
// data or a hash the receiver must resolve against its own local file
// (the existing relPath, which supplies the unchanged bytes). Returns the
// temp relative path.
func applyDeltaRoot(root *os.Root, relPath, peerID string, remoteFileSize int64, chunks []senderChunk) (string, error) {
	tmpRelPath := relPath + ".mesh-delta-tmp-" + peerID

	out, err := root.Create(tmpRelPath)
	if err != nil {
		return "", fmt.Errorf("create delta temp: %w", err)
	}

	old, err := root.Open(relPath)
	if err != nil {
		_ = out.Close()
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("open old file: %w", err)
	}

	if err := assembleDelta(out, old, remoteFileSize, chunks); err != nil {
		_ = out.Close()
		_ = old.Close()
		_ = root.Remove(tmpRelPath)
		return "", err
	}
	_ = old.Close()

	if err := out.Close(); err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("close delta temp: %w", err)
	}
	return tmpRelPath, nil
}

// assembleDelta writes the reconstructed file to out. Chunks must be
// sorted by offset and tile [0, remoteFileSize) exactly. Inline data
// is written verbatim; hash-only chunks are resolved by scanning the
// old file for a matching chunk and copying those bytes.
func assembleDelta(out, old *os.File, remoteFileSize int64, chunks []senderChunk) error {
	// Build a map: hash → (offset, length) from a single FastCDC pass
	// over the old file. We hash once; unchanged chunks look up here.
	oldChunks, err := ChunkFile(old)
	if err != nil {
		return fmt.Errorf("chunk old file: %w", err)
	}
	lookup := make(map[Hash256]Block, len(oldChunks))
	for _, b := range oldChunks {
		// First occurrence wins — duplicate content in the old file is
		// harmless to share across multiple remote chunks.
		if _, ok := lookup[b.Hash]; !ok {
			lookup[b.Hash] = b
		}
	}

	// Verify chunks tile [0, remoteFileSize) in offset order.
	var want int64
	for i, c := range chunks {
		if c.Offset != want {
			return fmt.Errorf("delta chunk %d offset=%d want %d (non-contiguous)", i, c.Offset, want)
		}
		if c.Length <= 0 {
			return fmt.Errorf("delta chunk %d non-positive length %d", i, c.Length)
		}
		want += int64(c.Length)
	}
	if want != remoteFileSize {
		return fmt.Errorf("delta chunks cover %d bytes, file_size=%d", want, remoteFileSize)
	}

	buf := make([]byte, fastCDCMax)
	for i, c := range chunks {
		if c.Data != nil {
			if len(c.Data) != c.Length {
				return fmt.Errorf("delta chunk %d data len=%d want length=%d", i, len(c.Data), c.Length)
			}
			if _, err := out.Write(c.Data); err != nil {
				return fmt.Errorf("write delta chunk %d: %w", i, err)
			}
			continue
		}
		local, ok := lookup[c.Hash]
		if !ok {
			return fmt.Errorf("delta chunk %d hash not in old file and no data", i)
		}
		if local.Length != c.Length {
			return fmt.Errorf("delta chunk %d length=%d local length=%d", i, c.Length, local.Length)
		}
		if _, err := old.Seek(local.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek old chunk %d: %w", i, err)
		}
		if _, err := io.ReadFull(old, buf[:local.Length]); err != nil {
			return fmt.Errorf("read old chunk %d: %w", i, err)
		}
		if _, err := out.Write(buf[:local.Length]); err != nil {
			return fmt.Errorf("write old chunk %d: %w", i, err)
		}
	}
	return nil
}

// hashFileRoot computes the SHA-256 hash of a file via an os.Root handle,
// preventing symlink TOCTOU (L5).
func hashFileRoot(root *os.Root, relPath string) (Hash256, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return Hash256{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Hash256{}, err
	}
	return hash256FromBytes(h.Sum(nil)), nil
}
