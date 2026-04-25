package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is a sessionDAGSource that returns a pre-loaded slice of
// rows under a mutex so tests can mutate the row set between handler
// poll ticks.
type fakeSource struct {
	mu    sync.Mutex
	rows  []json.RawMessage
	calls atomic.Int32
}

func (f *fakeSource) snapshot() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]json.RawMessage, len(f.rows))
	copy(out, f.rows)
	return out
}

func (f *fakeSource) append(row json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, row)
}

func (f *fakeSource) build(sessionID string) (*sessionDAG, error) {
	f.calls.Add(1)
	return buildSessionDAG(sessionID, f.snapshot())
}

// startSessionHandler spins up an httptest server whose only handler
// is serveSessionEvents bound to fs. Returns the SSE response (caller
// must close) plus a cancel func for graceful shutdown.
func startSessionHandler(t *testing.T, sessionID string, fs *fakeSource, poll time.Duration) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSessionEvents(r.Context(), w, sessionID, fs.build, poll)
	}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp, cancel
}

// readSSEEvent reads the next event block (up to a blank line)
// from r and parses it. Returns the event name and the raw data
// payload bytes. Comment lines (`: ping`) are skipped.
func readSSEEvent(t *testing.T, sc *bufio.Scanner, deadline time.Time) (event string, id string, data []byte, ok bool) {
	t.Helper()
	for time.Now().Before(deadline) {
		gotAny := false
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if gotAny {
					return event, id, data, true
				}
				// blank line with no content yet — keep waiting
				continue
			}
			gotAny = true
			switch {
			case strings.HasPrefix(line, ":"):
				// comment, ignore
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, strings.TrimPrefix(line, "data: ")...)
			}
		}
		// Scanner might have hit EOF or a temporary read; bail.
		return event, id, data, false
	}
	return "", "", nil, false
}

// --- dag_init delivery ---

func TestServeSessionEvents_SendsDagInitOnConnect(t *testing.T) {
	t.Parallel()
	fs := &fakeSource{
		rows: []json.RawMessage{
			reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
			respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		},
	}
	resp, cancel := startSessionHandler(t, "s1", fs, 50*time.Millisecond)
	defer cancel()
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	deadline := time.Now().Add(2 * time.Second)
	event, id, data, ok := readSSEEvent(t, sc, deadline)
	if !ok {
		t.Fatal("did not receive any event")
	}
	if event != sseEventDAGInit {
		t.Errorf("first event = %q, want %q", event, sseEventDAGInit)
	}
	if id != "1" {
		t.Errorf("event id = %q, want %q", id, "1")
	}
	var dag sessionDAG
	if err := json.Unmarshal(data, &dag); err != nil {
		t.Fatalf("unmarshal dag_init: %v\ndata=%s", err, data)
	}
	if dag.SessionID != "s1" {
		t.Errorf("session_id = %q, want s1", dag.SessionID)
	}
	if len(dag.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(dag.Nodes))
	}
	if len(dag.Branches) != 1 {
		t.Errorf("branches = %d, want 1", len(dag.Branches))
	}
}

func TestServeSessionEvents_DagInitOnEmptySession(t *testing.T) {
	t.Parallel()
	fs := &fakeSource{}
	resp, cancel := startSessionHandler(t, "empty", fs, 50*time.Millisecond)
	defer cancel()
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	event, _, data, ok := readSSEEvent(t, sc, time.Now().Add(2*time.Second))
	if !ok {
		t.Fatal("no event received")
	}
	if event != sseEventDAGInit {
		t.Errorf("event = %q, want %q", event, sseEventDAGInit)
	}
	var dag sessionDAG
	if err := json.Unmarshal(data, &dag); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dag.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(dag.Nodes))
	}
}

// --- live new-row detection (linear extension) ---

func TestServeSessionEvents_NodeAddedOnLinearExtension(t *testing.T) {
	t.Parallel()
	fs := &fakeSource{
		rows: []json.RawMessage{
			reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
			respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		},
	}
	resp, cancel := startSessionHandler(t, "s1", fs, 50*time.Millisecond)
	defer cancel()
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	deadline := time.Now().Add(3 * time.Second)
	// Drain dag_init.
	event, _, _, ok := readSSEEvent(t, sc, deadline)
	if !ok || event != sseEventDAGInit {
		t.Fatalf("expected dag_init, got event=%q ok=%v", event, ok)
	}

	// Append a turn-2 request after the handler has emitted dag_init.
	fs.append(reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")))
	fs.append(respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5))

	// Read until we get node_added or timeout.
	for time.Now().Before(deadline) {
		event, _, data, ok := readSSEEvent(t, sc, deadline)
		if !ok {
			t.Fatalf("did not receive node_added within deadline")
		}
		if event != sseEventNodeAdded {
			continue
		}
		var payload nodeAddedPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("unmarshal node_added: %v", err)
		}
		if payload.Node == nil {
			t.Fatalf("node missing in payload: %s", data)
		}
		if payload.Node.RowID != 2 {
			t.Errorf("node row_id = %d, want 2", payload.Node.RowID)
		}
		if len(payload.BranchChanges) == 0 {
			t.Errorf("expected branch_changes (tip_updated)")
		}
		// Linear extension → tip_updated
		found := false
		for _, c := range payload.BranchChanges {
			if c.Kind == "tip_updated" && c.NewTipID == payload.Node.BodyHash {
				found = true
			}
		}
		if !found {
			t.Errorf("missing tip_updated change: %+v", payload.BranchChanges)
		}
		return
	}
	t.Fatal("timed out waiting for node_added")
}

// --- live new-row detection (fork creates new branch) ---

func TestServeSessionEvents_NodeAddedOnFork(t *testing.T) {
	t.Parallel()
	fs := &fakeSource{
		rows: []json.RawMessage{
			reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
			respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
			reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
			respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
		},
	}
	resp, cancel := startSessionHandler(t, "s1", fs, 50*time.Millisecond)
	defer cancel()
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	deadline := time.Now().Add(3 * time.Second)
	// Drain dag_init.
	if event, _, _, ok := readSSEEvent(t, sc, deadline); !ok || event != sseEventDAGInit {
		t.Fatalf("expected dag_init")
	}

	// Fork from N1 (first request) with an alt continuation.
	fs.append(reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBodyWithUserOnly(1, "x", "alt")))

	for time.Now().Before(deadline) {
		event, _, data, ok := readSSEEvent(t, sc, deadline)
		if !ok {
			t.Fatal("timeout")
		}
		if event != sseEventNodeAdded {
			continue
		}
		var payload nodeAddedPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Fork should produce at least one new_branch change.
		hasNewBranch := false
		for _, c := range payload.BranchChanges {
			if c.Kind == "new_branch" {
				hasNewBranch = true
			}
		}
		if !hasNewBranch {
			t.Errorf("fork did not produce new_branch change: %+v", payload.BranchChanges)
		}
		return
	}
	t.Fatal("timed out")
}

// --- branch_renamed event ---

func TestServeSessionEvents_BranchRenamedOnMidChainFork(t *testing.T) {
	t.Parallel()
	// Initial linear chain N1→N2→N3→N4. Then a fork at N3 (mid-chain).
	// The pre-fork branch had branch_id derived from the chain root;
	// post-fork the branch_id changes because N3 became the fork
	// point. The handler must emit branch_renamed.
	fs := &fakeSource{
		rows: []json.RawMessage{
			reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
			respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
			reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
			respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
			reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBody(3, "x")),
			respRow(3, "r1", "2026-04-25T10:00:05Z", 200, "ok", 30, 5),
			reqRow(4, "r1", "2026-04-25T10:00:06Z", chatBody(4, "x")),
		},
	}
	resp, cancel := startSessionHandler(t, "s1", fs, 50*time.Millisecond)
	defer cancel()
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	deadline := time.Now().Add(3 * time.Second)
	if event, _, _, ok := readSSEEvent(t, sc, deadline); !ok || event != sseEventDAGInit {
		t.Fatalf("expected dag_init")
	}

	// Fork at N3 (turn 3 chat body) by sending an alt-extension off
	// the [u0,a0,u1,a1] state — that prefix is N3's parent state.
	fs.append(reqRow(5, "r1", "2026-04-25T10:00:08Z", chatBodyWithUserOnly(2, "x", "alt-after-turn-2")))

	for time.Now().Before(deadline) {
		event, _, data, ok := readSSEEvent(t, sc, deadline)
		if !ok {
			t.Fatal("timeout")
		}
		if event != sseEventNodeAdded {
			continue
		}
		var payload nodeAddedPayload
		_ = json.Unmarshal(data, &payload)
		// The chain through N4 now has a new fork ancestor (N3).
		// Its branch_id is renamed.
		hasRename := false
		for _, c := range payload.BranchChanges {
			if c.Kind == "branch_renamed" && c.OldBranchID != "" && c.NewBranchID != "" {
				hasRename = true
			}
		}
		if !hasRename {
			t.Errorf("expected branch_renamed change, got: %+v", payload.BranchChanges)
		}
		return
	}
	t.Fatal("timed out")
}

// --- reconnection: dag_init resent ---

func TestServeSessionEvents_ReconnectResendsDagInit(t *testing.T) {
	t.Parallel()
	// First connection: receive dag_init, drop. Second connection:
	// must get dag_init with the latest state.
	fs := &fakeSource{
		rows: []json.RawMessage{
			reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
			respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSessionEvents(r.Context(), w, "s1", fs.build, 50*time.Millisecond)
	}))
	defer srv.Close()

	read1 := func() string {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		req.Header.Set("Last-Event-ID", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		event, _, _, _ := readSSEEvent(t, sc, time.Now().Add(2*time.Second))
		return event
	}
	if got := read1(); got != sseEventDAGInit {
		t.Errorf("first connection event = %q, want dag_init", got)
	}

	// Add a node before reconnecting.
	fs.append(reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")))

	// Reconnect with Last-Event-ID; D8 says we still resend dag_init.
	if got := read1(); got != sseEventDAGInit {
		t.Errorf("reconnection event = %q, want dag_init", got)
	}
}

// --- diffSessionDAGs: no-op when no changes ---

func TestDiffSessionDAGs_NoChanges(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
	}
	d1, _ := buildSessionDAG("s", rows)
	d2, _ := buildSessionDAG("s", rows)
	added, changes := diffSessionDAGs(d1, d2)
	if len(added) != 0 {
		t.Errorf("expected no added nodes, got %d", len(added))
	}
	if len(changes) != 0 {
		t.Errorf("expected no branch changes, got %d", len(changes))
	}
}

// --- handler returns on context cancel ---

func TestServeSessionEvents_ReturnsOnContextCancel(t *testing.T) {
	t.Parallel()
	fs := &fakeSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSessionEvents(r.Context(), w, "s", fs.build, 20*time.Millisecond)
	}))
	defer srv.Close()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // let dag_init send (only sleep — we're testing cancel)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
}

// --- handleSessionEvents (production wiring) ---

func TestHandleSessionEvents_BadRequestOnMissingSID(t *testing.T) {
	t.Parallel()
	// Direct call with no path value yields the "missing session id"
	// branch. This mirrors what mux pattern enforcement guards against.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/sessions//events", nil)
	handleSessionEvents(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
