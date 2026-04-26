package main

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/mmdemirbas/mesh/internal/nodeutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

const (
	selfMonInterval        = 30 * time.Second
	goroutineWarnThreshold = 10000
	openFDWarnThreshold    = 10000
	stateMapWarnThreshold  = 10000
)

// startSelfMonitor periodically checks process-level metrics and logs warnings
// when thresholds are exceeded. Runs until ctx is cancelled.
func startSelfMonitor(ctx context.Context, log *slog.Logger) {
	defer nodeutil.RecoverPanic("cmd/mesh.startSelfMonitor")
	ticker := time.NewTicker(selfMonInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := runtime.NumGoroutine(); n > goroutineWarnThreshold {
				log.Warn("high goroutine count", "count", n, "threshold", goroutineWarnThreshold)
			}
			if fds := openFDCount(); fds >= 0 && fds > openFDWarnThreshold {
				log.Warn("high open file descriptor count", "count", fds, "threshold", openFDWarnThreshold)
			}
			comps, mets := state.Global.Sizes()
			if comps > stateMapWarnThreshold {
				log.Warn("large component state map", "size", comps, "threshold", stateMapWarnThreshold)
			}
			if mets > stateMapWarnThreshold {
				log.Warn("large metrics state map", "size", mets, "threshold", stateMapWarnThreshold)
			}
		}
	}
}
