package filesync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDeviceID_Shape(t *testing.T) {
	const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		id := generateDeviceID()
		if len(id) != deviceIDChars {
			t.Fatalf("len(id)=%d want %d (id=%q)", len(id), deviceIDChars, id)
		}
		for _, r := range id {
			if !strings.ContainsRune(crockfordAlphabet, r) {
				t.Fatalf("non-crockford rune %q in id %q", r, id)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id after %d iterations: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestFormatDeviceID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical", "ABCDE12345", "ABCDE-12345"},
		{"too-short", "ABCDE", "ABCDE"},
		{"too-long", "ABCDEFGHIJK", "ABCDEFGHIJK"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDeviceID(tc.in); got != tc.want {
				t.Fatalf("formatDeviceID(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDeviceID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"canonical", "ABCDE12345", "ABCDE12345", false},
		{"dashed", "ABCDE-12345", "ABCDE12345", false},
		{"lowercase", "abcde-12345", "ABCDE12345", false},
		{"mixed-ws", " abcde\t12345\n", "ABCDE12345", false},
		{"short", "ABCDE1234", "", true},
		{"long", "ABCDE123456", "", true},
		{"bad-char", "ABCDEI2345", "", true}, // 'I' not in Crockford
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDeviceID(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseDeviceID(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("parseDeviceID(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDeviceID_RoundTripGenerated(t *testing.T) {
	for i := 0; i < 32; i++ {
		id := generateDeviceID()
		got, err := parseDeviceID(formatDeviceID(id))
		if err != nil {
			t.Fatalf("parse(format(%q)) err=%v", id, err)
		}
		if got != id {
			t.Fatalf("round-trip: got %q want %q", got, id)
		}
	}
}

func TestLoadOrCreateDeviceID_CreatesAndPersists(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(first) != deviceIDChars {
		t.Fatalf("len(first)=%d want %d", len(first), deviceIDChars)
	}

	path := filepath.Join(dir, "device-id")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	if string(data) != first {
		t.Fatalf("persisted=%q want %q", data, first)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// On Windows the exact mode bits differ; only check the read/write bits on unix.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("device-id file is world/group accessible: mode=%#o", mode)
	}

	second, err := loadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second != first {
		t.Fatalf("second=%q want %q (persistence failed)", second, first)
	}
}

func TestLoadOrCreateDeviceID_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-id")
	if err := os.WriteFile(path, []byte("not-a-valid-id"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := loadOrCreateDeviceID(dir); err == nil {
		t.Fatal("loadOrCreateDeviceID accepted corrupt file; want error")
	}
}

func TestLoadOrCreateDeviceID_AcceptsLenientFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-id")
	// Persisted file contains a dashed, lowercase form with trailing newline.
	if err := os.WriteFile(path, []byte("abcde-12345\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, err := loadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id != "ABCDE12345" {
		t.Fatalf("id=%q want ABCDE12345", id)
	}
}
