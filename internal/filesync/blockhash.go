package filesync

import (
	"bytes"
	"crypto/sha256"
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
func applyDelta(oldPath, destPath string, blockSize, remoteFileSize int64, blocks []deltaBlock) (string, error) {
	tmpPath := destPath + ".mesh-delta-tmp"

	out, err := os.Create(tmpPath) //nolint:gosec // G304: destPath validated by caller
	if err != nil {
		return "", fmt.Errorf("create delta temp: %w", err)
	}
	closeOut := true
	defer func() {
		if closeOut {
			_ = out.Close()
		}
	}()

	// Build a map of changed blocks for O(1) lookup.
	changed := make(map[int64][]byte, len(blocks))
	for _, b := range blocks {
		changed[b.index] = b.data
	}

	// Open old file for reading unchanged blocks.
	old, err := os.Open(oldPath) //nolint:gosec // G304: oldPath validated by caller
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("open old file: %w", err)
	}
	defer func() { _ = old.Close() }()

	buf := make([]byte, blockSize)
	totalBlocks := (remoteFileSize + blockSize - 1) / blockSize
	for i := range totalBlocks {
		if data, ok := changed[i]; ok {
			// Use the delta block.
			if _, err := out.Write(data); err != nil {
				_ = os.Remove(tmpPath)
				return "", fmt.Errorf("write delta block %d: %w", i, err)
			}
		} else {
			// Copy unchanged block from old file.
			if _, err := old.Seek(i*blockSize, io.SeekStart); err != nil {
				_ = os.Remove(tmpPath)
				return "", fmt.Errorf("seek old block %d: %w", i, err)
			}
			n, err := io.ReadFull(old, buf)
			if n > 0 {
				if _, err := out.Write(buf[:n]); err != nil {
					_ = os.Remove(tmpPath)
					return "", fmt.Errorf("write old block %d: %w", i, err)
				}
			}
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				_ = os.Remove(tmpPath)
				return "", fmt.Errorf("read old block %d: %w", i, err)
			}
		}
	}

	// Truncate to exact remote file size (last block may be shorter).
	if err := out.Truncate(remoteFileSize); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("truncate: %w", err)
	}

	// Close before returning so caller can rename on Windows.
	closeOut = false
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close delta temp: %w", err)
	}

	return tmpPath, nil
}

// hashEqual compares two byte slices for equality.
func hashEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
