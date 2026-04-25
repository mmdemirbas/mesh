package gateway

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// --- CanonicalToolArg tests ---

// TestCanonicalToolArg_ReadCleansPath asserts the §4.5 rule: Read
// keys on filepath.Clean(file_path), so /a/b/../c and /a/c collide.
func TestCanonicalToolArg_ReadCleansPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{`{"file_path":"/a/b/../c"}`, "/a/c"},
		{`{"file_path":"/a/c"}`, "/a/c"},
		{`{"file_path":"/a//c"}`, "/a/c"},
		{`{"file_path":"/a/c/"}`, "/a/c"},
		{`{"file_path":"./relative"}`, "relative"},
	}
	for _, c := range cases {
		got := CanonicalToolArg("Read", json.RawMessage(c.in))
		if got != c.want {
			t.Errorf("Read %s: got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalToolArg_ReadIgnoresOffsetLimit: same file at different
// ranges still hashes the same. §4.5 explicitly drops offset/limit
// because re-reads of the same file at different byte ranges should
// be detectable.
func TestCanonicalToolArg_ReadIgnoresOffsetLimit(t *testing.T) {
	t.Parallel()
	a := CanonicalToolArg("Read", json.RawMessage(`{"file_path":"/foo","offset":1,"limit":100}`))
	b := CanonicalToolArg("Read", json.RawMessage(`{"file_path":"/foo","offset":500,"limit":50}`))
	if a != b {
		t.Errorf("offset/limit must not affect canonical key: %q != %q", a, b)
	}
}

// TestCanonicalToolArg_GrepCombinesPatternAndPath: pattern and path
// concatenated with NUL separator; path is filepath.Clean'd.
func TestCanonicalToolArg_GrepCombinesPatternAndPath(t *testing.T) {
	t.Parallel()
	got := CanonicalToolArg("Grep", json.RawMessage(`{"pattern":"foo.*","path":"/a/../b"}`))
	want := "foo.*" + "\x00" + "/b"
	if got != want {
		t.Errorf("Grep canonical = %q, want %q", got, want)
	}
}

// TestCanonicalToolArg_GrepEmptyPathTolerated: spec doesn't require
// path; pattern-only Greps still yield a stable key.
func TestCanonicalToolArg_GrepEmptyPathTolerated(t *testing.T) {
	t.Parallel()
	got := CanonicalToolArg("Grep", json.RawMessage(`{"pattern":"foo"}`))
	if got != "foo\x00" {
		t.Errorf("Grep with empty path = %q, want %q", got, "foo\x00")
	}
}

// TestCanonicalToolArg_BashCollapsesWhitespaceAndTruncates pins the
// 256-byte truncation rule. Two commands with identical first 256
// bytes (after whitespace collapse) hash the same. This is the
// known-imprecision case noted in §10.
func TestCanonicalToolArg_BashCollapsesWhitespaceAndTruncates(t *testing.T) {
	t.Parallel()
	// Whitespace collapse: tabs and runs of spaces become single space.
	a := CanonicalToolArg("Bash", json.RawMessage(`{"command":"git  status   --short"}`))
	b := CanonicalToolArg("Bash", json.RawMessage(`{"command":"git status --short"}`))
	if a != b {
		t.Errorf("whitespace collapse failed: %q != %q", a, b)
	}

	// Truncation at 256 bytes: long-prefix commands diverging only
	// past the cap collide. This is the known false-positive (§10).
	prefix := strings.Repeat("a", 250)
	a2 := CanonicalToolArg("Bash", json.RawMessage(`{"command":`+jsonString(prefix+" cmd1 X")+`}`))
	b2 := CanonicalToolArg("Bash", json.RawMessage(`{"command":`+jsonString(prefix+" cmd1 Y")+`}`))
	if a2 != b2 {
		t.Errorf("256-byte truncation expected to collide on long prefix; got distinct keys")
	}
}

// TestCanonicalToolArg_FallbackHashesRawInput: tools other than
// Read/Grep/Bash hash their raw input bytes.
func TestCanonicalToolArg_FallbackHashesRawInput(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"a":1,"b":"hello"}`)
	got := CanonicalToolArg("CustomTool", in)
	// Stable: same input → same hash.
	got2 := CanonicalToolArg("CustomTool", in)
	if got != got2 {
		t.Errorf("hash not stable: %q vs %q", got, got2)
	}
	// Recognizable shape: hex-encoded SHA-256 = 64 hex chars.
	if len(got) != 64 {
		t.Errorf("fallback key length = %d, want 64 (sha256 hex)", len(got))
	}
	// Different input → different hash.
	in2 := json.RawMessage(`{"a":2,"b":"hello"}`)
	if CanonicalToolArg("CustomTool", in2) == got {
		t.Errorf("different inputs hashed identical")
	}
	// Sanity: hex.DecodeString must accept it.
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("fallback key not valid hex: %v", err)
	}
}

// TestCanonicalToolArg_EmptyOrUnparseableReturnsEmpty: the §4.5
// "callers treat empty key as skip" contract.
func TestCanonicalToolArg_EmptyOrUnparseableReturnsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool string
		in   string
	}{
		{"Read", ``},
		{"Read", `{}`},
		{"Read", `{"file_path":""}`},
		{"Grep", `{}`},
		{"Bash", `{}`},
	}
	for _, c := range cases {
		got := CanonicalToolArg(c.tool, json.RawMessage(c.in))
		if got != "" {
			t.Errorf("%s %s: got %q, want empty", c.tool, c.in, got)
		}
	}
}
