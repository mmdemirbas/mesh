//go:build e2e || e2e_churn

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Eventually polls cond until it returns true or the deadline expires.
// Scenarios use it to wait for mesh state transitions — the SSH connection
// coming up, a forward listening, a file appearing on a peer. Failure is
// fatal and reports the last error message returned by the predicate.
//
// interval defaults to 200ms when zero. timeout defaults to 15s when zero.
func Eventually(ctx context.Context, t testing.TB, timeout, interval time.Duration, msg string, cond func() (bool, string)) {
	t.Helper()
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if interval == 0 {
		interval = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var lastDetail string
	for {
		ok, detail := cond()
		if ok {
			return
		}
		if detail != "" {
			lastDetail = detail
		}
		if time.Now().After(deadline) {
			if lastDetail == "" {
				t.Fatalf("e2e: %s: timed out after %s", msg, timeout)
			}
			t.Fatalf("e2e: %s: timed out after %s: %s", msg, timeout, lastDetail)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("e2e: %s: context cancelled: %v", msg, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// WaitForComponent waits until a component of the given type whose ID
// contains idSubstring reaches the requested status. idSubstring lets
// scenarios match dynamic IDs like "bastion [ssh-to-server] 0.0.0.0:2222"
// with a short stable anchor ("ssh-to-server").
func WaitForComponent(ctx context.Context, t testing.TB, node *Node, compType, idSubstring, status string, timeout time.Duration) ComponentState {
	t.Helper()
	var found ComponentState
	Eventually(ctx, t, timeout, 250*time.Millisecond,
		fmt.Sprintf("%s: %s/%s → %s", node.Alias, compType, idSubstring, status),
		func() (bool, string) {
			snap, err := node.AdminState(ctx)
			if err != nil {
				return false, err.Error()
			}
			var detail string
			for _, c := range snap.Components {
				if c.Type != compType {
					continue
				}
				if idSubstring != "" && !strings.Contains(c.ID, idSubstring) {
					continue
				}
				if c.Status == status {
					found = c
					return true, ""
				}
				detail = fmt.Sprintf("found %s/%s status=%s want=%s", c.Type, c.ID, c.Status, status)
			}
			if detail == "" {
				detail = fmt.Sprintf("no %s component matching %q", compType, idSubstring)
			}
			return false, detail
		})
	return found
}
