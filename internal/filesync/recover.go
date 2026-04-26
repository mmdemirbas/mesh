package filesync

import (
	"log/slog"
	"runtime/debug"
)

// recoverPanic absorbs a panic in a long-running goroutine and logs
// it at ERROR with the full stack. The surrounding goroutine returns
// normally, so paired sync.WaitGroup / mutex defers earlier in the
// stack still fire and the rest of the node keeps serving traffic.
//
// Use as the FIRST defer in every goroutine that processes peer-
// controlled or scan-derived input — `defer recoverPanic("scanLoop")`.
// The mesh node owns work the user trusts to a daemon while away
// from the keyboard; one bad request or a stale invariant from a
// disabled folder must not be a process-wide kill switch. Recovery
// + log is the documented Go pattern for this trade-off (`net/http`
// follows it for request handlers).
//
// Limitations: recovery cannot un-corrupt invariants. If the panic
// indicates a real deadlock or torn data structure, the recovered
// goroutine may keep producing wrong results until the next restart.
// The ERROR-level log is the operator's signal to redeploy.
func recoverPanic(where string) {
	if r := recover(); r != nil {
		slog.Error("goroutine panic recovered, node continues",
			"where", where, "panic", r, "stack", string(debug.Stack()))
	}
}
