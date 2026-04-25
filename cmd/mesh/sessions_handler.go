package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
)

// SSE event types per DESIGN_B2_live_session.local.md §4.2.
const (
	sseEventDAGInit   = "dag_init"
	sseEventNodeAdded = "node_added"
)

// branchChange is one item in a node_added event's branch_changes
// list. Per §4.4 (revised): tip_updated for linear extension,
// new_branch when a fork creates a new branch, branch_renamed when
// an existing branch's id changes because a fork appeared in its
// history.
type branchChange struct {
	Kind        string         `json:"kind"`
	BranchID    string         `json:"branch_id,omitempty"`
	NewTipID    string         `json:"new_tip_id,omitempty"`
	OldBranchID string         `json:"old_branch_id,omitempty"`
	NewBranchID string         `json:"new_branch_id,omitempty"`
	Branch      *sessionBranch `json:"branch,omitempty"`
}

// nodeAddedPayload is the JSON shape emitted on the node_added SSE
// event. The frontend reads payload.node and applies
// branch_changes to its in-memory state.
type nodeAddedPayload struct {
	Node          *sessionNode   `json:"node"`
	BranchChanges []branchChange `json:"branch_changes,omitempty"`
}

// sessionDAGSource is the function the handler calls each tick to
// rebuild the session DAG. Production code passes a closure that
// loads rows via gateway.AuditDirs(); tests pass a stub that returns
// rows from a controlled in-memory list.
type sessionDAGSource func(sessionID string) (*sessionDAG, error)

// globalSessionDAGCache is the package-level cache used by the
// handler. Sized per design D9.
var globalSessionDAGCache = newSessionDAGCache(dagCacheMax, dagCacheTTL)

// handleSessionEvents serves Server-Sent Events for a session id
// extracted from the URL path. Sends dag_init at connection start,
// then polls the audit log for new rows and emits node_added events
// with branch_changes diffs.
//
// Connection lifecycle is bounded by the request context: when the
// client disconnects (or the server shuts down), the handler returns
// and the polling loop stops.
func handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sid")
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	source := func(sid string) (*sessionDAG, error) {
		return loadSessionDAG(sid)
	}
	serveSessionEvents(r.Context(), w, sessionID, source, sessionPollInterval)
}

// loadSessionDAG is the production path: read all audit dirs, filter
// to sessionID, build the DAG. Used by the handler via
// sessionDAGSource.
func loadSessionDAG(sessionID string) (*sessionDAG, error) {
	dirs := gateway.AuditDirs()
	rows, err := loadSessionRowsFromDirs(dirs, sessionID)
	if err != nil {
		return nil, err
	}
	return buildSessionDAG(sessionID, rows)
}

// serveSessionEvents is the testable core of handleSessionEvents.
// All HTTP and audit-log machinery is injected via parameters so
// tests can drive it without spinning up a Recorder.
func serveSessionEvents(ctx context.Context, w http.ResponseWriter, sessionID string, source sessionDAGSource, poll time.Duration) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied
	w.WriteHeader(http.StatusOK)

	emit := func(event, id string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if id != "" {
			if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Build initial DAG and send dag_init. On reconnect (Last-Event-ID
	// set by the browser) we still resend the full state per D8.
	cur, err := source(sessionID)
	if err != nil {
		http.Error(w, "build session dag: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if cur == nil {
		cur = &sessionDAG{SessionID: sessionID}
	}
	globalSessionDAGCache.Put(sessionID, cur)

	initEventID := lastNodeRowID(cur)
	if err := emit(sseEventDAGInit, strconv.FormatUint(initEventID, 10), cur); err != nil {
		return
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	// Heartbeat: an SSE comment line every 30 s keeps middleboxes
	// (proxies, load balancers) from closing the connection on idle
	// sessions. Browsers ignore comment lines.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			next, err := source(sessionID)
			if err != nil || next == nil {
				continue
			}
			added, changes := diffSessionDAGs(cur, next)
			if len(added) == 0 && len(changes) == 0 {
				continue
			}
			// Emit one node_added event per new node. The first
			// event carries any branch_changes (renames, new
			// branches); subsequent events for nodes added in the
			// same tick repeat tip_updated changes scoped to their
			// own row. For the common case (one new node per tick)
			// this is a single event with full branch_changes.
			eventsByNode := splitChangesByNode(added, changes, next)
			for _, n := range added {
				ev := eventsByNode[n.BodyHash]
				payload := nodeAddedPayload{Node: n, BranchChanges: ev}
				if err := emit(sseEventNodeAdded, strconv.FormatUint(n.RowID, 10), payload); err != nil {
					return
				}
			}
			cur = next
			globalSessionDAGCache.Put(sessionID, next)
		}
	}
}

// lastNodeRowID returns the highest RowID in the DAG, or 0 when the
// DAG has no nodes. Used as the SSE event id for dag_init so the
// browser's Last-Event-ID header on reconnect is meaningful.
func lastNodeRowID(d *sessionDAG) uint64 {
	if d == nil {
		return 0
	}
	var max uint64
	for _, n := range d.Nodes {
		if n.RowID > max {
			max = n.RowID
		}
	}
	return max
}

// diffSessionDAGs returns the nodes present in next but not in pre,
// plus the branch_changes implied by going from pre to next.
//
// Branch matching: a post branch P is "the same logical branch" as a
// pre branch Q when Q's tip_id appears anywhere in the chain from
// P.RootID up to P.TipID (inclusive). This catches three cases:
//
//   - Linear extension at length ≥ 2 — Q.tip_id matches some
//     ancestor of P.tip_id; Q.branch_id == P.branch_id (no rename),
//     P.tip_id != Q.tip_id (tip_updated).
//   - Linear extension from a length-1 chain — Q.tip_id == Q.root_id
//     and is now a non-tip ancestor of P; the branch_id format
//     changes per §4.3 (":root" → ":id(second_node)") so a rename
//     event also fires.
//   - Mid-chain fork — Q.tip_id == P.tip_id (the leaf survived) but
//     P.branch_id includes a closer fork point; emits rename.
//
// A post branch with no Q match is genuinely new (new_branch).
//
// Each pre branch is matched to at most one post branch; the first
// post branch whose chain contains Q.tip_id wins. A second post
// branch reaching the same Q.tip_id (via a sibling chain) is treated
// as new.
func diffSessionDAGs(pre, next *sessionDAG) (added []*sessionNode, changes []branchChange) {
	preNodeIDs := make(map[string]struct{}, len(pre.Nodes))
	for _, n := range pre.Nodes {
		preNodeIDs[n.BodyHash] = struct{}{}
	}
	for _, n := range next.Nodes {
		if _, ok := preNodeIDs[n.BodyHash]; !ok {
			added = append(added, n)
		}
	}

	preByID := make(map[string]*sessionBranch, len(pre.Branches))
	preByTip := make(map[string]*sessionBranch, len(pre.Branches))
	for _, b := range pre.Branches {
		preByID[b.BranchID] = b
		preByTip[b.TipID] = b
	}
	nextNodes := make(map[string]*sessionNode, len(next.Nodes))
	for _, n := range next.Nodes {
		nextNodes[n.BodyHash] = n
	}

	// matchedPre tracks pre branches already paired to a post
	// branch — prevents one pre branch from being matched twice
	// when two post branches share an ancestor.
	matchedPre := make(map[string]struct{}, len(pre.Branches))
	// preToPostBranch is the resolved pre→post mapping for branches
	// that were not renamed (same branch_id) plus those that were
	// renamed (different branch_id, same logical chain).
	preToPostBranch := make(map[string]string, len(pre.Branches))

	for _, post := range next.Branches {
		var matched *sessionBranch
		// Walk from post.TipID up via ParentID until we reach
		// post.RootID (the fork point or chain root) or run out of
		// ancestors. Each visited node is checked against preByTip.
		// Stop at the first match — that pre branch is the
		// predecessor of post.
		cur := post.TipID
		visited := 0
		for cur != "" {
			if cand, ok := preByTip[cur]; ok {
				if _, alreadyTaken := matchedPre[cand.BranchID]; !alreadyTaken {
					matched = cand
					break
				}
			}
			if cur == post.RootID {
				break
			}
			node := nextNodes[cur]
			if node == nil {
				break
			}
			cur = node.ParentID
			visited++
			if visited > len(next.Nodes)+1 {
				// Defensive — should never trip; bound prevents
				// infinite loops on malformed parent pointers.
				break
			}
		}

		if matched == nil {
			// Genuinely new branch.
			if _, sameID := preByID[post.BranchID]; !sameID {
				bcopy := *post
				changes = append(changes, branchChange{Kind: "new_branch", Branch: &bcopy})
			}
			continue
		}

		matchedPre[matched.BranchID] = struct{}{}
		preToPostBranch[matched.BranchID] = post.BranchID

		if matched.BranchID != post.BranchID {
			changes = append(changes, branchChange{
				Kind:        "branch_renamed",
				OldBranchID: matched.BranchID,
				NewBranchID: post.BranchID,
			})
		}
		if matched.TipID != post.TipID {
			changes = append(changes, branchChange{
				Kind:     "tip_updated",
				BranchID: post.BranchID,
				NewTipID: post.TipID,
			})
		}
	}

	return added, changes
}

// splitChangesByNode groups branch_changes by the node responsible
// for them. The per-tick diff produces one logical event per new
// node; for common single-node ticks the entire changes slice goes
// onto that one node. For multi-node ticks (rare — only on
// reconnection or first connect after a backlog), the
// branch_changes list is attached to the LAST added node so the
// frontend processes node-by-node first and then applies all branch
// metadata at the end.
//
// Simpler v1: attach all changes to the last new node. The frontend
// state model in §5.1 is fine with that — branches are derived from
// nodes anyway.
func splitChangesByNode(added []*sessionNode, changes []branchChange, _ *sessionDAG) map[string][]branchChange {
	out := make(map[string][]branchChange, len(added))
	if len(added) == 0 || len(changes) == 0 {
		return out
	}
	last := added[len(added)-1]
	out[last.BodyHash] = changes
	return out
}
