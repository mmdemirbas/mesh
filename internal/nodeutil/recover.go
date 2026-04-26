package nodeutil

import (
	"log/slog"
	"runtime/debug"
)

// RecoverPanic absorbs a panic in a long-running goroutine and logs
// it at ERROR with the full stack. The surrounding goroutine returns
// normally, so paired sync.WaitGroup / mutex defers earlier in the
// stack still fire and the rest of the node keeps serving traffic.
//
// Use as the FIRST defer in every goroutine that processes peer-
// controlled or network-controlled input — `defer nodeutil.RecoverPanic("ssh.handleSession")`.
// Mesh runs as a daemon the user trusts to stay up while away from
// the keyboard; one bad request or a stale invariant must not be
// a process-wide kill switch.
//
// Limitation: recovery cannot un-corrupt invariants. The ERROR-level
// log is the operator's signal to redeploy.
func RecoverPanic(where string) {
	if r := recover(); r != nil {
		slog.Error("goroutine panic recovered, node continues",
			"where", where, "panic", r, "stack", string(debug.Stack()))
	}
}
