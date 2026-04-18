package filesync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const (
	// defaultBlockSize is 128 KB — balances granularity vs. overhead.
	// Smaller blocks catch more fine-grained changes but increase the
	// number of hashes exchanged. 128 KB is a common choice in rsync-like tools.
	defaultBlockSize = 128 * 1024
)

// computeBlockSignatures reads a file and returns SHA-256 hashes of each
// sequential fixed-size block. The last block may be smaller than blockSize.
func computeBlockSignatures(path string, blockSize int64) ([][]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path validated by caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBlockSigs(f, blockSize)
}

// computeBlockSignaturesRoot is the os.Root-safe variant of computeBlockSignatures.
func computeBlockSignaturesRoot(root *os.Root, relPath string, blockSize int64) ([][]byte, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBlockSigs(f, blockSize)
}

func readBlockSigs(f *os.File, blockSize int64) ([][]byte, error) {
	var hashes [][]byte
	buf := make([]byte, blockSize)
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			h := sha256.Sum256(buf[:n])
			hashes = append(hashes, h[:])
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read block: %w", err)
		}
	}
	return hashes, nil
}

// computeDeltaBlocks reads a file and returns only the blocks whose SHA-256
// does not match the corresponding entry in localHashes. Blocks beyond the
// length of localHashes (file grew) are always included.
func computeDeltaBlocks(path string, blockSize int64, localHashes [][]byte) ([]deltaBlock, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path validated by caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readDeltaBlocks(f, blockSize, localHashes)
}

// computeDeltaBlocksRoot is the os.Root-safe variant of computeDeltaBlocks.
func computeDeltaBlocksRoot(root *os.Root, relPath string, blockSize int64, localHashes [][]byte) ([]deltaBlock, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readDeltaBlocks(f, blockSize, localHashes)
}

func readDeltaBlocks(f *os.File, blockSize int64, localHashes [][]byte) ([]deltaBlock, error) {
	var blocks []deltaBlock
	buf := make([]byte, blockSize)
	idx := 0
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			h := sha256.Sum256(buf[:n])
			// Include this block if it's new or differs from the local version.
			include := idx >= len(localHashes) || !hashEqual(h[:], localHashes[idx])
			if include {
				data := make([]byte, n)
				copy(data, buf[:n])
				blocks = append(blocks, deltaBlock{index: int64(idx), data: data})
			}
		}
		idx++
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read block: %w", err)
		}
	}
	return blocks, nil
}

type deltaBlock struct {
	index int64
	data  []byte
}

// applyDelta reconstructs a file by copying unchanged blocks from the old file
// and overwriting changed blocks from the delta. Returns the path to a temp file.
// F5: peerID is appended to the temp name to prevent concurrent peers from
// clobbering each other's delta temp files for the same path.
func applyDelta(oldPath, destPath, peerID string, blockSize, remoteFileSize int64, blocks []deltaBlock) (string, error) {
	tmpPath := destPath + ".mesh-delta-tmp-" + peerID

	out, err := os.Create(tmpPath) //nolint:gosec // G304: destPath validated by caller
	if err != nil {
		return "", fmt.Errorf("create delta temp: %w", err)
	}

	old, err := os.Open(oldPath) //nolint:gosec // G304: oldPath validated by caller
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("open old file: %w", err)
	}

	if err := assembleDelta(out, old, blockSize, remoteFileSize, blocks); err != nil {
		_ = out.Close()
		_ = old.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	_ = old.Close()

	// Close before returning so caller can rename on Windows.
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close delta temp: %w", err)
	}

	return tmpPath, nil
}

// applyDeltaRoot is the os.Root-safe variant of applyDelta.
// Returns the relative temp path within root.
// F5: peerID prevents concurrent peers from clobbering the same temp file.
func applyDeltaRoot(root *os.Root, relPath, peerID string, blockSize, remoteFileSize int64, blocks []deltaBlock) (string, error) {
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

	if err := assembleDelta(out, old, blockSize, remoteFileSize, blocks); err != nil {
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

// assembleDelta writes the reconstructed file to out by copying unchanged
// blocks from old and overwriting changed blocks from the delta slice.
func assembleDelta(out, old *os.File, blockSize, remoteFileSize int64, blocks []deltaBlock) error {
	changed := make(map[int64][]byte, len(blocks))
	for _, b := range blocks {
		changed[b.index] = b.data
	}

	buf := make([]byte, blockSize)
	totalBlocks := (remoteFileSize + blockSize - 1) / blockSize
	// F6: track the old file's logical read position to skip unnecessary
	// seeks when blocks are read sequentially.
	oldPos := int64(0)
	for i := range totalBlocks {
		if data, ok := changed[i]; ok {
			if _, err := out.Write(data); err != nil {
				return fmt.Errorf("write delta block %d: %w", i, err)
			}
			// old file wasn't read — position unchanged, but next
			// unchanged block needs a seek because we skipped one.
		} else {
			want := i * blockSize
			if oldPos != want {
				if _, err := old.Seek(want, io.SeekStart); err != nil {
					return fmt.Errorf("seek old block %d: %w", i, err)
				}
			}
			n, err := io.ReadFull(old, buf)
			if n > 0 {
				if _, err := out.Write(buf[:n]); err != nil {
					return fmt.Errorf("write old block %d: %w", i, err)
				}
			}
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return fmt.Errorf("read old block %d: %w", i, err)
			}
			oldPos = want + int64(n)
		}
	}

	if err := out.Truncate(remoteFileSize); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}

// hashFileRoot computes the SHA-256 hash of a file via an os.Root handle,
// preventing symlink TOCTOU (L5).
func hashFileRoot(root *os.Root, relPath string) (string, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashEqual compares two byte slices for equality.
func hashEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
