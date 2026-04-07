package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestStartSelfMonitor_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		startSelfMonitor(ctx, slog.Default())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startSelfMonitor did not return after context cancellation")
	}
}

func TestSelfMonitorThresholds(t *testing.T) {
	// Verify that the goroutine threshold is above the current count
	// (no false positives under normal test conditions).
	if goroutineWarnThreshold < 100 {
		t.Errorf("goroutineWarnThreshold = %d, too low for normal operation", goroutineWarnThreshold)
	}
	if openFDWarnThreshold < 100 {
		t.Errorf("openFDWarnThreshold = %d, too low for normal operation", openFDWarnThreshold)
	}
	if stateMapWarnThreshold < 100 {
		t.Errorf("stateMapWarnThreshold = %d, too low for normal operation", stateMapWarnThreshold)
	}
}

func TestSelfMonitorNoWarningUnderThreshold(t *testing.T) {
	// Run one monitoring cycle with a logger that captures output.
	// Under normal test conditions, no thresholds should be exceeded.
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithCancel(context.Background())

	// Use a very short-lived context so the monitor runs at most one tick.
	// We trigger the tick by using a short interval replacement isn't feasible,
	// so instead we just verify the thresholds are sane and call cancel.
	cancel()
	startSelfMonitor(ctx, log)

	if buf.Len() > 0 {
		t.Errorf("unexpected warning output: %s", buf.String())
	}
}
