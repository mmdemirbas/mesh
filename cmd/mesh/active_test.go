package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
)

func TestHandleActiveSnapshot_EmptyRegistry(t *testing.T) {
	t.Parallel()
	// Use a private snapshot read by sampling Active. Other parallel
	// tests may register entries; the assertion only checks shape +
	// counts ≥ 0.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/active", nil)
	handleActiveSnapshot(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp activeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ByPhase == nil || resp.ByGateway == nil {
		t.Errorf("by_phase and by_gateway should be non-nil maps")
	}
}

func TestHandleActiveSnapshot_CountsByPhaseAndGateway(t *testing.T) {
	t.Parallel()
	// Use unique high IDs to avoid collisions with other parallel
	// tests against the package-level Active.
	ids := []uint64{0xB4_0001, 0xB4_0002, 0xB4_0003}
	gateway.Active.Register(&gateway.ActiveRequest{ID: ids[0], Gateway: "alpha"})
	gateway.Active.UpdatePhase(ids[0], "upstream_processing", time.Now())
	gateway.Active.Register(&gateway.ActiveRequest{ID: ids[1], Gateway: "alpha"})
	gateway.Active.UpdatePhase(ids[1], "upstream_processing", time.Now())
	gateway.Active.Register(&gateway.ActiveRequest{ID: ids[2], Gateway: "beta"})
	gateway.Active.UpdatePhase(ids[2], "mesh_to_client", time.Now())
	defer func() {
		for _, id := range ids {
			gateway.Active.Unregister(id)
		}
	}()

	w := httptest.NewRecorder()
	handleActiveSnapshot(w, httptest.NewRequest(http.MethodGet, "/api/gateway/active", nil))
	var resp activeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ByGateway["alpha"] < 2 {
		t.Errorf("by_gateway[alpha] = %d, want ≥ 2", resp.ByGateway["alpha"])
	}
	if resp.ByGateway["beta"] < 1 {
		t.Errorf("by_gateway[beta] = %d, want ≥ 1", resp.ByGateway["beta"])
	}
	if resp.ByPhase["upstream_processing"] < 2 {
		t.Errorf("by_phase[upstream_processing] = %d, want ≥ 2", resp.ByPhase["upstream_processing"])
	}
	if resp.ByPhase["mesh_to_client"] < 1 {
		t.Errorf("by_phase[mesh_to_client] = %d, want ≥ 1", resp.ByPhase["mesh_to_client"])
	}
}

func TestServeActiveStream_EmitsStateOnChange(t *testing.T) {
	t.Parallel()
	id := uint64(0xB4_1000)
	gateway.Active.Register(&gateway.ActiveRequest{ID: id, Gateway: "gw", StartedAt: time.Now()})
	defer gateway.Active.Unregister(id)
	gateway.Active.UpdatePhase(id, "client_to_mesh", time.Now())

	var emits int32
	var sawState, sawCompleted atomic.Bool
	emit := func(event string, _ any) error {
		atomic.AddInt32(&emits, 1)
		if event == "state" {
			sawState.Store(true)
		}
		if event == "completed" {
			sawCompleted.Store(true)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		initial, _ := gateway.Active.SnapshotByID(id)
		serveActiveStream(ctx, id, initial, emit, 25*time.Millisecond)
		close(done)
	}()
	// Drive a phase change and a byte-count change.
	time.Sleep(40 * time.Millisecond)
	gateway.Active.UpdatePhase(id, "upstream_processing", time.Now())
	time.Sleep(40 * time.Millisecond)
	gateway.Active.AddBytesToClient(id, 1024)
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if !sawState.Load() {
		t.Errorf("expected at least one state event; emits=%d", atomic.LoadInt32(&emits))
	}
	if sawCompleted.Load() {
		t.Errorf("did not expect completed event before unregister")
	}
}

func TestServeActiveStream_EmitsCompletedOnUnregister(t *testing.T) {
	t.Parallel()
	id := uint64(0xB4_2000)
	gateway.Active.Register(&gateway.ActiveRequest{ID: id, Gateway: "gw"})
	gateway.Active.UpdatePhase(id, "client_to_mesh", time.Now())

	var sawCompleted atomic.Bool
	emit := func(event string, _ any) error {
		if event == "completed" {
			sawCompleted.Store(true)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		initial, _ := gateway.Active.SnapshotByID(id)
		serveActiveStream(ctx, id, initial, emit, 20*time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	gateway.Active.Unregister(id)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("serveActiveStream did not return after Unregister")
	}
	if !sawCompleted.Load() {
		t.Errorf("expected completed event on unregister")
	}
}

func TestHandleActiveEvents_UnknownIDReturnsCompleted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inject a path value the way the mux pattern would.
		r.SetPathValue("id", "999999")
		handleActiveEvents(w, r)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (SSE always 200, content reports completion)", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	deadline := time.Now().Add(2 * time.Second)
	var sawCompleted bool
	for time.Now().Before(deadline) && sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: completed") {
			sawCompleted = true
			break
		}
	}
	if !sawCompleted {
		t.Errorf("expected completed event for unknown id")
	}
}

func TestHandleActiveEvents_BadIDReturns400(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/gateway/active//events", nil)
	r.SetPathValue("id", "not-a-number")
	handleActiveEvents(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSameSnapshot_EqualVsChanged(t *testing.T) {
	t.Parallel()
	a := gateway.ActiveRequestSnapshot{CurrentSegment: "x", BytesUpstream: 1, BytesDownstream: 2, BytesToClient: 3, UpstreamModel: "m"}
	b := a
	if !sameSnapshot(a, b) {
		t.Errorf("equal snapshots should return true")
	}
	b.BytesToClient = 4
	if sameSnapshot(a, b) {
		t.Errorf("changed BytesToClient should return false")
	}
	b = a
	b.CurrentSegment = "y"
	if sameSnapshot(a, b) {
		t.Errorf("changed CurrentSegment should return false")
	}
}
