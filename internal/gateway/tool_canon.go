package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// CanonicalToolArg returns the canonical key for a tool invocation,
// used by repeat_reads in commit 8 to detect re-reads of the same
// resource. The canonicalization rules (§4.5) are deliberately
// per-tool:
//
//   - "Read"  → filepath.Clean(file_path); offset/limit ignored so the
//     same file at different ranges still counts as a re-read.
//   - "Grep"  → pattern + "\x00" + filepath.Clean(path); the NUL
//     separator prevents pathological collisions where pattern bleeds
//     into path.
//   - "Bash"  → first 256 bytes of command after whitespace collapse;
//     accepted false-positive rate for long-prefix commands like
//     `curl <huge-url> | grep X` (see §10 caveats).
//   - any other tool → SHA-256(raw input bytes). Stable for identical
//     model output but drifts if the model emits the same logical
//     input with different JSON key order (§4.5 known false-miss).
//
// Returns "" when the input is empty or unparseable for the named
// tool — callers treat empty key as "skip".
func CanonicalToolArg(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	switch toolName {
	case "Read":
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.FilePath == "" {
			return ""
		}
		return filepath.Clean(args.FilePath)
	case "Grep":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Pattern == "" {
			return ""
		}
		path := args.Path
		if path != "" {
			path = filepath.Clean(path)
		}
		return args.Pattern + "\x00" + path
	case "Bash":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Command == "" {
			return ""
		}
		return collapseBashCommand(args.Command, 256)
	default:
		h := sha256.Sum256(input)
		return hex.EncodeToString(h[:])
	}
}

// collapseBashCommand trims surrounding whitespace, collapses internal
// runs of whitespace to a single space, and truncates to maxBytes. The
// truncation is byte-level (no rune awareness) since the result is
// hashed downstream and exact codepoint boundaries do not matter.
func collapseBashCommand(cmd string, maxBytes int) string {
	cmd = strings.TrimSpace(cmd)
	var b strings.Builder
	b.Grow(len(cmd))
	prevSpace := false
	for _, r := range cmd {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	out := b.String()
	if len(out) > maxBytes {
		out = out[:maxBytes]
	}
	return out
}
