package filesync

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// resolveConflict determines which version of a file wins (local vs remote)
// and returns the winner ("local" or "remote") along with the conflict file
// path for the loser. Does NOT rename or modify any files — the caller handles
// the file operations (B13: download must succeed before any rename).
//
// Strategy: newer mtime wins. The loser is saved as:
//
//	<name>.sync-conflict-<YYYYMMDD-HHMMSS>-<deviceShort>.<ext>
func resolveConflict(folderRoot, relPath string, localMtimeNS, remoteMtimeNS int64, remoteDeviceID string) (winner, conflictPath string) {
	localPath := filepath.Join(folderRoot, filepath.FromSlash(relPath))

	// Use the file's current on-disk mtime rather than the scan-time value —
	// the user may have edited the file between scan and resolution, and
	// renaming the (newly-latest) local copy to a conflict file would silently
	// discard their edits. Fall back to the passed-in index mtime on stat
	// error so we still make a reasonable decision.
	if info, statErr := os.Stat(localPath); statErr == nil {
		localMtimeNS = info.ModTime().UnixNano()
	}

	// Determine winner by mtime. If equal, remote wins to avoid data loss.
	if localMtimeNS > remoteMtimeNS {
		return "local", ""
	}

	// Remote wins — compute conflict name for the local file.
	// N7: check for collision and append a counter if needed, since
	// second-granularity timestamps can collide under rapid edits.
	conflictName := conflictFileName(relPath, remoteDeviceID)
	cPath := filepath.Join(folderRoot, filepath.FromSlash(conflictName))
	if _, err := os.Stat(cPath); err == nil {
		// Collision — try up to 99 suffixed names.
		base := cPath
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		found := false
		for i := 2; i <= 100; i++ {
			candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				cPath = candidate
				found = true
				break
			}
		}
		if !found {
			// All 99 counter names taken — fall back to random suffix.
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			cPath = stem + "-" + hex.EncodeToString(b) + ext
		}
	}

	return "remote", cPath
}

// conflictFileName generates a Syncthing-style conflict file name.
// Example: "docs/report.docx" -> "docs/report.sync-conflict-20260406-143022-abc123.docx"
func conflictFileName(relPath, deviceID string) string {
	dir := filepath.Dir(relPath)
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	ts := time.Now().Format("20060102-150405")
	short := deviceID
	if len(short) > 6 {
		short = short[:6]
	}

	conflictBase := fmt.Sprintf("%s.sync-conflict-%s-%s%s", name, ts, short, ext)

	if dir == "." {
		return conflictBase
	}
	return filepath.ToSlash(filepath.Join(dir, conflictBase))
}

// conflictMarkerRe validates the conflict marker portion between
// ".sync-conflict-" and the extension (or end of string).
var conflictMarkerRe = regexp.MustCompile(`^\d{8}-\d{6}-[a-zA-Z0-9]+(-\d+|-[a-fA-F0-9]+)?$`)

// OriginalPathFromConflict extracts the original file path from a conflict
// file name by stripping the ".sync-conflict-YYYYMMDD-HHMMSS-DEVICE" marker.
// Returns the original relative path and true, or ("", false) if the name
// does not match the conflict pattern.
func OriginalPathFromConflict(conflictRelPath string) (string, bool) {
	dir := filepath.Dir(conflictRelPath)
	base := filepath.Base(conflictRelPath)

	const marker = ".sync-conflict-"
	idx := strings.LastIndex(base, marker)
	if idx <= 0 {
		return "", false
	}

	origName := base[:idx]
	suffix := base[idx+len(marker):]

	// The suffix is either "YYYYMMDD-HHMMSS-DEVICE[.ext]" or
	// "YYYYMMDD-HHMMSS-DEVICE(-N|-HEX)[.ext]".
	// Split off a potential extension: find the LAST dot in the suffix
	// that comes AFTER the device portion.
	// The original extension is the same as what follows the conflict marker.
	ext := ""
	if dotIdx := strings.LastIndex(suffix, "."); dotIdx >= 0 {
		ext = suffix[dotIdx:]
		suffix = suffix[:dotIdx]
	}

	if !conflictMarkerRe.MatchString(suffix) {
		return "", false
	}

	return joinOriginal(dir, origName+ext), true
}

func joinOriginal(dir, name string) string {
	if dir == "." {
		return name
	}
	return filepath.ToSlash(filepath.Join(dir, name))
}

// --- Conflict diff types and computation ---

const (
	maxDiffFileSize = 1 << 20 // 1 MB per side
	maxDiffLines    = 500
	binaryProbeSize = 8192
)

// ConflictDiff contains the comparison between a conflict file and its original.
type ConflictDiff struct {
	ConflictPath   string     `json:"conflict_path"`
	OriginalPath   string     `json:"original_path"`
	OriginalExists bool       `json:"original_exists"`
	IsBinary       bool       `json:"is_binary"`
	Conflict       FileMeta   `json:"conflict"`
	Original       FileMeta   `json:"original"`
	Lines          []DiffLine `json:"lines,omitempty"`
	Truncated      bool       `json:"truncated"`
}

// FileMeta holds file metadata for one side of a conflict diff.
type FileMeta struct {
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
	SHA256 string    `json:"sha256"`
}

// DiffLine represents one line in a unified-style diff.
type DiffLine struct {
	Op   string `json:"op"`   // "equal", "add", "delete"
	Text string `json:"text"`
}

// ComputeConflictDiff compares a conflict file against its original on disk.
func ComputeConflictDiff(folderRoot, conflictRel string) (ConflictDiff, error) {
	origRel, ok := OriginalPathFromConflict(conflictRel)
	if !ok {
		return ConflictDiff{}, fmt.Errorf("not a conflict file: %s", conflictRel)
	}

	conflictAbs := filepath.Join(folderRoot, filepath.FromSlash(conflictRel))
	originalAbs := filepath.Join(folderRoot, filepath.FromSlash(origRel))

	// Path traversal guard.
	cleanRoot := filepath.Clean(folderRoot) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(conflictAbs)+string(filepath.Separator), cleanRoot) ||
		!strings.HasPrefix(filepath.Clean(originalAbs)+string(filepath.Separator), cleanRoot) {
		return ConflictDiff{}, fmt.Errorf("path escapes folder root")
	}

	result := ConflictDiff{
		ConflictPath: conflictRel,
		OriginalPath: origRel,
	}

	// Stat the conflict file (must exist).
	cInfo, err := os.Stat(conflictAbs)
	if err != nil {
		return ConflictDiff{}, fmt.Errorf("conflict file: %w", err)
	}
	result.Conflict = FileMeta{Size: cInfo.Size(), Mtime: cInfo.ModTime()}

	// Stat the original file (may be missing).
	oInfo, err := os.Stat(originalAbs)
	if err != nil {
		result.OriginalExists = false
		return result, nil
	}
	result.OriginalExists = true
	result.Original = FileMeta{Size: oInfo.Size(), Mtime: oInfo.ModTime()}

	// Binary detection: check first 8 KB of each file.
	cProbe, err := readHead(conflictAbs, binaryProbeSize)
	if err != nil {
		return ConflictDiff{}, fmt.Errorf("reading conflict file: %w", err)
	}
	oProbe, err := readHead(originalAbs, binaryProbeSize)
	if err != nil {
		return ConflictDiff{}, fmt.Errorf("reading original file: %w", err)
	}

	if bytes.ContainsRune(cProbe, 0) || bytes.ContainsRune(oProbe, 0) {
		result.IsBinary = true
		result.Conflict.SHA256 = hashFileSHA256(conflictAbs)
		result.Original.SHA256 = hashFileSHA256(originalAbs)
		return result, nil
	}

	// Text diff: read up to maxDiffFileSize per side.
	cText, cTrunc, err := readTextCapped(conflictAbs, maxDiffFileSize)
	if err != nil {
		return ConflictDiff{}, fmt.Errorf("reading conflict file: %w", err)
	}
	oText, oTrunc, err := readTextCapped(originalAbs, maxDiffFileSize)
	if err != nil {
		return ConflictDiff{}, fmt.Errorf("reading original file: %w", err)
	}
	result.Truncated = cTrunc || oTrunc

	cLines := splitLines(cText)
	oLines := splitLines(oText)

	result.Lines, result.Truncated = diffLines(oLines, cLines, maxDiffLines)
	if cTrunc || oTrunc {
		result.Truncated = true
	}
	return result, nil
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	nr, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:nr], nil
}

func hashFileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func readTextCapped(path string, maxBytes int) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	data := make([]byte, maxBytes+1)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, err
	}
	truncated := n > maxBytes
	if truncated {
		n = maxBytes
	}
	return string(data[:n]), truncated, nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// Remove trailing empty element from final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines computes a minimal edit script between two line slices using the
// Myers O(ND) algorithm. It returns at most maxOut DiffLine entries and a
// truncated flag.
func diffLines(a, b []string, maxOut int) ([]DiffLine, bool) {
	n, m := len(a), len(b)

	// Trivial cases.
	if n == 0 && m == 0 {
		return nil, false
	}
	if n == 0 {
		lines := make([]DiffLine, 0, min(m, maxOut))
		for i := 0; i < m && len(lines) < maxOut; i++ {
			lines = append(lines, DiffLine{Op: "add", Text: b[i]})
		}
		return lines, len(lines) < m
	}
	if m == 0 {
		lines := make([]DiffLine, 0, min(n, maxOut))
		for i := 0; i < n && len(lines) < maxOut; i++ {
			lines = append(lines, DiffLine{Op: "delete", Text: a[i]})
		}
		return lines, len(lines) < n
	}

	// Hash lines for faster comparison.
	ah := hashLines(a)
	bh := hashLines(b)

	// Myers diff on hashes.
	editScript := myersDiff(ah, bh)

	// Convert edit script to DiffLine entries.
	var lines []DiffLine
	truncated := false
	ai, bi := 0, 0
	for _, op := range editScript {
		if len(lines) >= maxOut {
			truncated = true
			break
		}
		switch op {
		case '=':
			lines = append(lines, DiffLine{Op: "equal", Text: a[ai]})
			ai++
			bi++
		case '-':
			lines = append(lines, DiffLine{Op: "delete", Text: a[ai]})
			ai++
		case '+':
			lines = append(lines, DiffLine{Op: "add", Text: b[bi]})
			bi++
		}
	}
	return lines, truncated
}

func hashLines(lines []string) []uint64 {
	out := make([]uint64, len(lines))
	h := fnv.New64a()
	for i, l := range lines {
		h.Reset()
		_, _ = h.Write([]byte(l))
		out[i] = h.Sum64()
	}
	return out
}

// myersDiff implements the Myers O(ND) difference algorithm. It returns an
// edit script as a byte slice where '=' means equal, '-' means delete from a,
// and '+' means insert from b.
func myersDiff(a, b []uint64) []byte {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil
	}

	// V array indexed from -max to max; offset by max.
	size := 2*max + 1
	v := make([]int, size)
	// Parent trace for backtracking.
	type point struct{ x, y int }
	trace := make([][]int, 0, max+1)

	for d := 0; d <= max; d++ {
		// Save current V state for backtracking.
		saved := make([]int, size)
		copy(saved, v)
		trace = append(trace, saved)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
				x = v[k+1+max] // move down
			} else {
				x = v[k-1+max] + 1 // move right
			}
			y := x - k

			// Follow diagonal (equal elements).
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}

			v[k+max] = x

			if x >= n && y >= m {
				// Backtrack to build the edit script.
				return backtrack(trace, d, n, m, max)
			}
		}
	}
	// Should not reach here for valid inputs.
	return nil
}

func backtrack(trace [][]int, d, n, m, max int) []byte {
	var script []byte
	x, y := n, m
	for d > 0 {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK+max]
		prevY := prevX - prevK

		// Diagonal moves (equal).
		for x > prevX && y > prevY {
			script = append(script, '=')
			x--
			y--
		}

		if k == prevK+1 {
			// Right move = delete from a.
			script = append(script, '-')
		} else {
			// Down move = insert from b.
			script = append(script, '+')
		}
		x = prevX
		y = prevY
		d--
	}

	// Remaining diagonal at d=0.
	for x > 0 && y > 0 {
		script = append(script, '=')
		x--
		y--
	}

	// Reverse the script (we built it backwards).
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}
