package main

import (
	"runtime"
	"testing"
)

func TestOpenFDCount(t *testing.T) {
	n := openFDCount()
	if runtime.GOOS == "windows" {
		if n != -1 {
			t.Errorf("openFDCount() = %d on Windows, want -1", n)
		}
		return
	}
	// On Unix, a running process always has at least stdin/stdout/stderr open.
	if n < 3 {
		t.Errorf("openFDCount() = %d, want >= 3", n)
	}
}
