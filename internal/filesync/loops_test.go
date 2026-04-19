package filesync

import (
	"context"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
)

// These tests pin goroutine-lifecycle invariants of the live loops: they
// must exit when ctx is cancelled, whether blocked on an internal gate
// (firstScanDone) or a ticker. Breaking this produces goroutine leaks
// that survive mesh restarts and show up only under long-running stress.

// minimalNode builds a Node with enough wiring to drive syncLoop/scanLoop
// without peers, folders, TLS, or HTTP servers. syncAllPeers becomes a
// no-op because folders is empty.
func minimalNode() *Node {
	return &Node{
		cfg:           config.FilesyncCfg{MaxConcurrent: 1},
		folders:       map[string]*folderState{},
		scanTrigger:   make(chan struct{}, 1),
		firstScanDone: make(chan struct{}),
	}
}

func TestSyncLoop_CtxCancelBeforeFirstScan(t *testing.T) {
	t.Parallel()
	n := minimalNode()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		n.syncLoop(ctx)
		close(done)
	}()

	// Loop is parked on firstScanDone. Cancelling ctx must unblock it.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncLoop did not exit on ctx cancel before first scan")
	}
}

func TestSyncLoop_CtxCancelAfterFirstScan(t *testing.T) {
	t.Parallel()
	n := minimalNode()
	ctx, cancel := context.WithCancel(context.Background())

	// Simulate first-scan completion.
	close(n.firstScanDone)

	done := make(chan struct{})
	go func() {
		n.syncLoop(ctx)
		close(done)
	}()

	// Let the loop run one iteration of syncAllPeers (empty folders = no-op)
	// and enter the select on ticker/scanTrigger/ctx.Done.
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncLoop did not exit on ctx cancel after first scan")
	}
}

func TestSyncLoop_RespondsToScanTrigger(t *testing.T) {
	t.Parallel()
	n := minimalNode()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	close(n.firstScanDone)

	// Count syncAllPeers invocations via a side channel: folders is empty,
	// so syncAllPeers returns immediately. Observe progress by sending on
	// scanTrigger and confirming the loop wakes and returns to select on
	// the next trigger. We do this by draining scanTrigger (buffered size 1)
	// twice: the first send may race with the initial post-firstScanDone
	// syncAllPeers call, the second is guaranteed to trigger a new cycle.
	done := make(chan struct{})
	go func() {
		n.syncLoop(ctx)
		close(done)
	}()

	// Fire two triggers; with buffer=1, the second may block until the
	// first is consumed. That's exactly what we want to verify.
	deadline := time.Now().Add(2 * time.Second)
	for consumed := 0; consumed < 2; {
		select {
		case n.scanTrigger <- struct{}{}:
			consumed++
		default:
			if time.Now().After(deadline) {
				t.Fatal("syncLoop is not draining scanTrigger within deadline")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncLoop did not exit on ctx cancel")
	}
}

func TestScanLoop_CtxCancelExits(t *testing.T) {
	t.Parallel()
	n := minimalNode()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		// No watcher: dirtyCh stays nil and the only wakeup is ticker or ctx.
		n.scanLoop(ctx, 10*time.Second, nil)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanLoop did not exit on ctx cancel")
	}
}

func TestScanLoop_CtxCancelExitsWithWatcher(t *testing.T) {
	t.Parallel()
	n := minimalNode()
	ctx, cancel := context.WithCancel(context.Background())

	// A folderWatcher with a live dirtyCh — nil is also a valid state for
	// scanLoop so separating these cases catches a "forgot to handle nil
	// watcher" and a "forgot to handle watcher.dirtyCh closed" regression.
	w := &folderWatcher{
		dirtyCh:    make(chan struct{}, 1),
		dirtyRoots: make(map[string]bool),
	}

	done := make(chan struct{})
	go func() {
		n.scanLoop(ctx, 10*time.Second, w)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanLoop did not exit on ctx cancel (with watcher)")
	}
}
