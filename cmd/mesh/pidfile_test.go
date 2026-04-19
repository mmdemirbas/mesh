package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setFakeHome points UserHomeDir at a temp dir so pid/port files land in
// an isolated sandbox. Returns the resulting run dir.
func setFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // Unix
	t.Setenv("USERPROFILE", home) // Windows
	return filepath.Join(home, ".mesh", "run")
}

// TestPidFile_RoundTrip covers the full pidfile lifecycle: write reads back
// our own pid, and remove deletes the file so the next read errors.
func TestPidFile_RoundTrip(t *testing.T) {
	runPath := setFakeHome(t)

	const node = "alpha"
	if err := writePidFile(node); err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	want := filepath.Join(runPath, "mesh-alpha.pid")
	if got := pidFilePath(node); got != want {
		t.Errorf("pidFilePath = %q, want %q", got, want)
	}

	pid, err := readPidFile(node)
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("readPidFile = %d, want %d (own pid)", pid, os.Getpid())
	}

	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Unix: check 0600. Windows does not honor this.
	if perm := info.Mode().Perm(); perm != 0600 && perm != 0666 {
		t.Errorf("pid file perm = %o, want 0600 (or 0666 on Windows)", perm)
	}

	removePidFile(node)
	if _, err := readPidFile(node); err == nil {
		t.Error("readPidFile after remove: expected error, got nil")
	}
}

// TestReadPidFile_MalformedContent pins the contract: non-numeric file
// contents must return an error, not 0.
func TestReadPidFile_MalformedContent(t *testing.T) {
	runPath := setFakeHome(t)
	if err := os.MkdirAll(runPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "mesh-bad.pid"), []byte("not a number"), 0600); err != nil {
		t.Fatal(err)
	}

	pid, err := readPidFile("bad")
	if err == nil {
		t.Errorf("readPidFile = %d, nil; want error", pid)
	}
}

// TestResolveNodesForDown_ExplicitArgs: explicit args must pass through
// unchanged, bypassing pidfile and config lookup entirely.
func TestResolveNodesForDown_ExplicitArgs(t *testing.T) {
	setFakeHome(t)
	got := resolveNodesForDown([]string{"one", "two"}, "/nonexistent.yaml")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("resolveNodesForDown = %v, want [one two]", got)
	}
}

// TestResolveNodesForDown_FromPidFiles: when args are empty, pid files in
// the run dir seed the node list. Order is sorted.
func TestResolveNodesForDown_FromPidFiles(t *testing.T) {
	runPath := setFakeHome(t)
	if err := os.MkdirAll(runPath, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if err := os.WriteFile(filepath.Join(runPath, "mesh-"+name+".pid"), []byte("1"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	got := resolveNodesForDown(nil, "/nonexistent.yaml")
	want := []string{"alpha", "bravo", "charlie"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("resolveNodesForDown = %v, want %v", got, want)
	}
}

// TestResolveNodesForDown_EmptyWhenNothing: no pid files and no config
// returns nil rather than panicking.
func TestResolveNodesForDown_EmptyWhenNothing(t *testing.T) {
	setFakeHome(t)
	got := resolveNodesForDown(nil, "/definitely/does/not/exist.yaml")
	if got != nil {
		t.Errorf("resolveNodesForDown = %v, want nil", got)
	}
}

// TestWritePidFile_ContentIsDecimalPid sanity-checks the on-disk format:
// plain decimal ASCII, no trailing newline.
func TestWritePidFile_ContentIsDecimalPid(t *testing.T) {
	setFakeHome(t)
	if err := writePidFile("x"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFilePath("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(string(data)); err != nil {
		t.Errorf("pid file content %q does not parse as int: %v", data, err)
	}
}
