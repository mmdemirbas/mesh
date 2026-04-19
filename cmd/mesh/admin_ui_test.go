package main

import (
	"strings"
	"testing"
)

// TestAdminUIRendersPeerBackoff pins the R3 dashboard surface: the filesync
// peer row must render an explicit "backing off" indicator when the API
// reports a non-zero `backoff_remaining`. If this test fails, an operator
// can no longer tell "peer is being actively skipped by the retry tracker"
// apart from "peer had a transient error" apart from "file is quarantined".
func TestAdminUIRendersPeerBackoff(t *testing.T) {
	t.Parallel()
	mustContain := []string{
		// Reads the JSON field the filesync API exposes.
		"p.backoff_remaining",
		// Surfaces it as a distinct, labelled badge.
		"backing off",
		// Converts nanoseconds (Go time.Duration JSON encoding) to ms
		// before handing to the existing fmtElapsed helper.
		"p.backoff_remaining/1e6",
	}
	for _, sub := range mustContain {
		if !strings.Contains(adminUI, sub) {
			t.Errorf("adminUI missing %q — R3 peer-backoff indicator regressed", sub)
		}
	}
}
