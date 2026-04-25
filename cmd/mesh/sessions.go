package main

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// B2 Live Session View — backend DAG construction.
//
// Each Claude conversation request observed by mesh becomes a node in a
// session-scoped DAG. Nodes are identified by a hash of the request body
// (D1, D2). Parents are derived by longest-prefix-match against the
// messages array (D4). Branches are identified by their fork point and
// the first child after it (D6) so that linear extension does not change
// a branch id.
//
// Scope: pure data manipulation. No HTTP wiring here; that lives in the
// session SSE handler in admin.go (B2.2).
//
// See docs/gateway/DESIGN_B2_live_session.local.md for the design pass.

// nodeIDHexLen is the truncated hex length of node ids and the
// internal canonical hashes. 16 hex chars = 64 bits — well below
// collision risk for any real session size (D1).
const nodeIDHexLen = 16

// dagCacheTTL drops cached per-session DAGs whose last access is
// older than this. Re-derivation on the next request is fast for any
// session that fits in audit retention (D9).
const dagCacheTTL = 5 * time.Minute

// dagCacheMax bounds the number of session DAGs kept in memory.
// Loopback admin server, one operator typically — generous (D9).
const dagCacheMax = 32

// sessionPollInterval is the cadence at which the SSE handler
// rescans the audit log for new rows in a session. Half-second hits
// the design's "see new message immediately" feel without burning
// CPU on idle sessions.
const sessionPollInterval = 500 * time.Millisecond

// sessionNode is one observed conversation state. Identified by
// BodyHash (sha256 of the wire-byte request body, truncated to
// nodeIDHexLen hex chars). CanonicalHash is the hash of the body's
// canonical-JSON re-marshaling — used internally for parent matching
// (D3) and never surfaced to the frontend.
type sessionNode struct {
	BodyHash      string `json:"id"`
	CanonicalHash string `json:"-"`
	ParentID      string `json:"parent_id,omitempty"`
	BranchID      string `json:"branch_id"`
	RowID         uint64 `json:"row_id"`
	Run           string `json:"run"`
	TS            string `json:"ts"`

	RequestSummary  nodeRequestSummary  `json:"request_summary"`
	ResponseSummary nodeResponseSummary `json:"response_summary"`

	// prefixCanonicalHashes[k] is the canonical hash of this node's
	// body with messages truncated to the first k items. Cached at
	// scan time so the parent algorithm avoids re-marshaling (§2.5).
	prefixCanonicalHashes []string

	// messageCount is the original messages-array length, before any
	// truncation. Determines the upper bound of prefix hashes worth
	// computing during parent search.
	messageCount int
}

// nodeRequestSummary carries the per-node request fields the frontend
// needs to render a chat bubble or DAG node label.
type nodeRequestSummary struct {
	Model        string `json:"model,omitempty"`
	MappedModel  string `json:"mapped_model,omitempty"`
	Direction    string `json:"direction,omitempty"`
	Stream       bool   `json:"stream,omitempty"`
	MessageCount int    `json:"message_count"`
}

// nodeResponseSummary carries the per-node response fields. Populated
// from a paired resp row when present; left zero-valued when no
// response is on disk yet (request still in flight, or resp row was
// rolled out of the audit window).
type nodeResponseSummary struct {
	Status       int    `json:"status,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	ElapsedMs    int64  `json:"elapsed_ms,omitempty"`
	HasResponse  bool   `json:"has_response"`
}

// sessionBranch is a chain from a fork point to a leaf. Branch
// identity (BranchID) is stable under linear extension and changes
// only when a new fork is introduced in this branch's history (D6).
type sessionBranch struct {
	BranchID       string `json:"branch_id"`
	RootID         string `json:"root_id"`
	ParentBranchID string `json:"parent_branch_id,omitempty"`
	CreatedAt      string `json:"created_at"`
	LastActivityAt string `json:"last_activity_at"`
	NodeCount      int    `json:"node_count"`
	TipID          string `json:"tip_id"`
}

// sessionDAG is the assembled output for one session id.
type sessionDAG struct {
	SessionID       string           `json:"session_id"`
	Nodes           []*sessionNode   `json:"nodes"`
	Branches        []*sessionBranch `json:"branches"`
	DefaultBranchID string           `json:"default_branch_id,omitempty"`
}

// nodeIDFromBody computes the body-hash node id (D1, D2). The input
// is the raw body bytes as stored in the audit row (json.RawMessage
// for valid-JSON bodies — the wire bytes verbatim).
func nodeIDFromBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:nodeIDHexLen]
}

// canonicalBodyHash decodes body, marshals it back through the
// canonical pipeline (sorted keys, no HTML escape, no extra
// whitespace, json.Number to preserve number text), and returns the
// hash of the canonical bytes (D3, D11).
//
// Returns an error only when body is not valid JSON. Callers treat
// that as "node has no canonical hash" and skip prefix matching for
// it (§2.7).
func canonicalBodyHash(body []byte) (string, error) {
	v, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return "", err
	}
	return canonicalHashOf(v)
}

// prefixCanonicalHashes computes the canonical hash of body with
// messages truncated to k items, for each k in [0, messageCount].
// Returns nil when body cannot be parsed as JSON (§2.7) — caller
// treats as "no parent matching" and the node becomes an orphan
// root.
//
// The slice is indexed by k. prefixCanonicalHashes[k] is the
// canonical hash of body-with-messages-truncated-to-k. The slice
// has length messageCount+1; prefixCanonicalHashes[messageCount]
// equals the canonical hash of the full body.
func prefixCanonicalHashes(body []byte) (hashes []string, messageCount int, err error) {
	v, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return nil, 0, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		// Top-level JSON is not an object — no messages array to
		// truncate. Treat as orphan: no prefix hashes. The node's
		// body_hash still works for identity.
		return nil, 0, nil
	}
	messages, _ := obj["messages"].([]any)
	messageCount = len(messages)
	hashes = make([]string, messageCount+1)
	for k := 0; k <= messageCount; k++ {
		// Build a shallow copy with messages truncated to k items.
		// All other top-level keys are preserved; nested values
		// (system, tools, etc.) participate in the hash so two
		// requests with the same messages-prefix but different
		// system prompt are correctly distinguished (§2.6).
		shallow := make(map[string]any, len(obj))
		for key, val := range obj {
			shallow[key] = val
		}
		// Slice is fine — we only read messages, never write back.
		shallow["messages"] = messages[:k]
		h, herr := canonicalHashOf(shallow)
		if herr != nil {
			return nil, 0, herr
		}
		hashes[k] = h
	}
	return hashes, messageCount, nil
}

// decodeJSONPreservingNumbers decodes raw into a Go value tree using
// json.Number for numeric leaves. Preserves the original numeric text
// across re-marshal so two bodies that differ only in whitespace or
// key order canonicalize identically while bodies that differ in
// numeric form (e.g. "1" vs "1.0") do not collide.
func decodeJSONPreservingNumbers(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// canonicalHashOf marshals v through the canonical pipeline and
// returns sha256[:nodeIDHexLen] of the bytes.
func canonicalHashOf(v any) (string, error) {
	canonical, err := canonicalMarshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:nodeIDHexLen], nil
}

// canonicalMarshal emits v as JSON with sorted keys (Go's encoding/json
// default for map[string]any), no HTML escaping, and no trailing
// newline. The Encoder wrapper is the standard way to disable HTML
// escaping for sub-objects without affecting global Marshal behavior.
func canonicalMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// rawAuditRow is the minimal shape sessions.go needs to build a DAG.
// Both req rows (with body, model, etc.) and resp rows (with status,
// outcome, usage) deserialize into this struct so we can iterate the
// JSONL stream once.
type rawAuditRow struct {
	T           string          `json:"t"`
	ID          uint64          `json:"id"`
	Run         string          `json:"run"`
	TS          string          `json:"ts"`
	SessionID   string          `json:"session_id"`
	Model       string          `json:"model"`
	MappedModel string          `json:"mapped_model"`
	Direction   string          `json:"direction"`
	Stream      bool            `json:"stream"`
	Body        json.RawMessage `json:"body"`

	// resp-row-only fields
	Status    int              `json:"status"`
	Outcome   string           `json:"outcome"`
	ElapsedMs int64            `json:"elapsed_ms"`
	Usage     *json.RawMessage `json:"usage"`
	StreamSum *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"stream_summary"`
}

// usageWire is the shape of the resp row's usage field, used to pull
// token counts into the response summary. Mirrors gateway.Usage but
// kept local so sessions.go has no cross-package coupling beyond the
// JSONL wire format.
type usageWire struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// buildSessionDAG runs the parent algorithm and branch derivation
// against a slice of audit rows already filtered to one session.
// Rows arrive in any order; the function sorts by id ascending so
// "prior" means "lower id" (chronological by capture order).
//
// Both req and resp rows may be present. Req rows produce nodes;
// resp rows enrich the node sharing the same (id, run) pair. A req
// without a resp produces a node with HasResponse=false.
func buildSessionDAG(sessionID string, rawRows []json.RawMessage) (*sessionDAG, error) {
	type pairKey struct {
		id  uint64
		run string
	}
	// Parse rows once; group req and resp by (id, run).
	type parsedRow struct {
		raw rawAuditRow
	}
	reqRows := make(map[pairKey]parsedRow)
	respRows := make(map[pairKey]parsedRow)
	for _, raw := range rawRows {
		var r rawAuditRow
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		key := pairKey{r.ID, r.Run}
		switch r.T {
		case "req":
			reqRows[key] = parsedRow{r}
		case "resp":
			respRows[key] = parsedRow{r}
		}
	}

	// Sort req row keys by id ascending. Tie-break by run for
	// determinism across mesh restarts that share a session id.
	keys := make([]pairKey, 0, len(reqRows))
	for k := range reqRows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].id != keys[j].id {
			return keys[i].id < keys[j].id
		}
		return keys[i].run < keys[j].run
	})

	// First pass: build nodes, compute hashes.
	nodes := make([]*sessionNode, 0, len(keys))
	for _, k := range keys {
		req := reqRows[k].raw
		body := []byte(req.Body)
		// json.RawMessage may carry a JSON-string-encoded body when
		// the original body was non-JSON. The body_hash is over the
		// stored bytes verbatim — two wire-identical non-JSON bodies
		// produce equal stored bytes, so equal hashes (V1).
		bodyHash := nodeIDFromBody(body)
		canHash, _ := canonicalBodyHash(body)
		prefixHashes, msgCount, _ := prefixCanonicalHashes(body)

		n := &sessionNode{
			BodyHash:              bodyHash,
			CanonicalHash:         canHash,
			RowID:                 req.ID,
			Run:                   req.Run,
			TS:                    req.TS,
			prefixCanonicalHashes: prefixHashes,
			messageCount:          msgCount,
			RequestSummary: nodeRequestSummary{
				Model:        req.Model,
				MappedModel:  req.MappedModel,
				Direction:    req.Direction,
				Stream:       req.Stream,
				MessageCount: msgCount,
			},
		}
		if resp, ok := respRows[k]; ok {
			n.ResponseSummary.HasResponse = true
			n.ResponseSummary.Status = resp.raw.Status
			n.ResponseSummary.Outcome = resp.raw.Outcome
			n.ResponseSummary.ElapsedMs = resp.raw.ElapsedMs
			if resp.raw.Usage != nil {
				var u usageWire
				if json.Unmarshal(*resp.raw.Usage, &u) == nil {
					n.ResponseSummary.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
					n.ResponseSummary.OutputTokens = u.OutputTokens
				}
			} else if resp.raw.StreamSum != nil && resp.raw.StreamSum.Usage != nil {
				n.ResponseSummary.InputTokens = resp.raw.StreamSum.Usage.InputTokens
				n.ResponseSummary.OutputTokens = resp.raw.StreamSum.Usage.OutputTokens
			}
		}
		nodes = append(nodes, n)
	}

	// Second pass: parent matching. canonicalToNode maps a canonical
	// hash to the most recent prior node carrying it. Iterating in
	// chronological order means an overwrite is the "newest match
	// wins" rule (D5). We index by canonical hash so prefix
	// matching against a later node's truncated form lands on the
	// right prior (D3).
	canonicalToNode := make(map[string]*sessionNode, len(nodes))
	for _, n := range nodes {
		if n.prefixCanonicalHashes == nil {
			// Malformed body — orphan root (§2.7).
			canonicalToNode[n.CanonicalHash] = n
			continue
		}
		// Try prefix lengths from len-1 down to 0. The full-body
		// case (k = messageCount) would match the node itself if
		// already inserted; we have not inserted yet, so it can
		// only match an identical prior — still correct.
		for k := n.messageCount - 1; k >= 0; k-- {
			prefixHash := n.prefixCanonicalHashes[k]
			if parent, ok := canonicalToNode[prefixHash]; ok {
				n.ParentID = parent.BodyHash
				break
			}
		}
		// Insert AFTER lookup so a node never matches itself as
		// parent on the messageCount-prefix case.
		canonicalToNode[n.CanonicalHash] = n
	}

	// Third pass: derive branches.
	branches := deriveBranches(nodes)

	// Default branch: most recent activity (§3.4).
	dag := &sessionDAG{
		SessionID: sessionID,
		Nodes:     nodes,
		Branches:  branches,
	}
	dag.DefaultBranchID = pickDefaultBranchID(branches)
	return dag, nil
}

// deriveBranches walks the parent pointers to assign every node a
// BranchID under the §4.3 scheme:
//
//	branch_id = id(fork_point) + ":" + id(first_node_after_fork_point)
//
// The fork point of a chain is the most recent ancestor with
// multiple children. For chains without any forking ancestor, the
// fork point is the chain's root and the first-node-after-fork-point
// is the second node in the chain (or the special ":root" suffix
// for length-1 chains).
//
// One branch entry is emitted per leaf; siblings under a common
// fork point produce sibling branches with different ids.
func deriveBranches(nodes []*sessionNode) []*sessionBranch {
	if len(nodes) == 0 {
		return nil
	}
	byID := make(map[string]*sessionNode, len(nodes))
	children := make(map[string][]*sessionNode, len(nodes))
	var leaves []*sessionNode
	for _, n := range nodes {
		byID[n.BodyHash] = n
	}
	hasChild := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.ParentID != "" {
			children[n.ParentID] = append(children[n.ParentID], n)
			hasChild[n.ParentID] = true
		}
	}
	for _, n := range nodes {
		if !hasChild[n.BodyHash] {
			leaves = append(leaves, n)
		}
	}

	// A branch_id is determined by walking up from each leaf to find
	// the most recent fork point. A node is a fork point when it
	// has more than one child. The chain's fork-point ancestor is
	// either that node, or — if the leaf's chain has none — the
	// chain's root.
	branches := make([]*sessionBranch, 0, len(leaves))
	branchByID := make(map[string]*sessionBranch, len(leaves))

	for _, leaf := range leaves {
		// Walk to root, recording the path.
		path := []*sessionNode{leaf}
		cur := leaf
		for cur.ParentID != "" {
			parent := byID[cur.ParentID]
			if parent == nil {
				break
			}
			path = append(path, parent)
			cur = parent
		}
		// path is leaf -> ... -> root (last element).
		root := path[len(path)-1]
		// Find the most recent fork point: the highest-index
		// element of path whose children-count > 1. "Highest index"
		// in the leaf-to-root list means closest to the leaf.
		var forkIdx = -1
		for i := 0; i < len(path); i++ {
			if len(children[path[i].BodyHash]) > 1 {
				forkIdx = i
				break
			}
		}

		var branchID, rootID string
		switch {
		case forkIdx >= 0:
			// Fork point exists somewhere in the chain. The branch
			// runs from path[forkIdx] (fork point) to leaf. The
			// first-node-after-fork-point on this branch is
			// path[forkIdx-1] when the leaf is below the fork
			// point, or — when forkIdx == 0 (the leaf itself is a
			// fork point) — the special ":root" suffix because
			// the leaf is its own first-after-fork.
			fork := path[forkIdx]
			rootID = fork.BodyHash
			if forkIdx == 0 {
				branchID = fork.BodyHash + ":root"
			} else {
				branchID = fork.BodyHash + ":" + path[forkIdx-1].BodyHash
			}
		default:
			// No fork in chain. Root is the chain's root.
			rootID = root.BodyHash
			if len(path) == 1 {
				// Length-1 chain — the leaf IS the root.
				branchID = root.BodyHash + ":root"
			} else {
				// path[len-1] is root, path[len-2] is the second
				// node.
				branchID = root.BodyHash + ":" + path[len(path)-2].BodyHash
			}
		}

		// Walk path bottom-up to assign BranchID to every node on
		// the branch (i.e. from leaf up to but not including the
		// node above the fork point — those nodes belong to ancestor
		// branches).
		var branchEnd int
		switch {
		case forkIdx >= 0:
			branchEnd = forkIdx // include fork point as branch root
		default:
			branchEnd = len(path) - 1 // include chain root
		}
		for i := 0; i <= branchEnd; i++ {
			path[i].BranchID = branchID
		}

		// Compose the branch metadata. Multiple leaves can produce
		// the same branch_id only when the algorithm is wrong — by
		// construction, distinct leaves under a common fork point
		// have distinct first-children. Defensive dedupe regardless.
		if existing, ok := branchByID[branchID]; ok {
			// Update activity-at if this leaf is more recent.
			if leaf.TS > existing.LastActivityAt {
				existing.LastActivityAt = leaf.TS
				existing.TipID = leaf.BodyHash
			}
			continue
		}
		b := &sessionBranch{
			BranchID:       branchID,
			RootID:         rootID,
			CreatedAt:      path[branchEnd].TS,
			LastActivityAt: leaf.TS,
			NodeCount:      branchEnd + 1,
			TipID:          leaf.BodyHash,
		}
		branches = append(branches, b)
		branchByID[branchID] = b
	}

	// Compute parent_branch_id for forked branches.
	//
	// A branch's id is `id(fork_point):id(first_after_fork_point)`.
	// At a fork point with multiple children, the *oldest* child
	// (lowest row id) marks the original chain — the one that
	// existed before any fork. Sibling branches with newer first-
	// after-fork-points are fork-offs of the original; their
	// parent_branch_id is the original sibling's branch_id.
	//
	// The original branch's own parent_branch_id stays empty unless
	// the fork point is itself a non-root, non-fork-point node on a
	// higher-level original branch — in that case the parent points
	// to whichever branch contains the fork point's parent. The
	// simple lookup `byID[fork.ParentID].BranchID` is correct
	// because non-fork-point ancestors carry exactly one BranchID
	// (assigned during chain walk), and a higher-level fork point
	// would itself be a branch root tracked here.
	for _, b := range branches {
		fork := byID[b.RootID]
		if fork == nil {
			continue
		}
		siblings := children[fork.BodyHash]
		if len(siblings) > 1 {
			// Sort siblings oldest-first by row id (tie-break by
			// run for cross-restart determinism).
			sortedSiblings := append([]*sessionNode(nil), siblings...)
			sort.Slice(sortedSiblings, func(i, j int) bool {
				if sortedSiblings[i].RowID != sortedSiblings[j].RowID {
					return sortedSiblings[i].RowID < sortedSiblings[j].RowID
				}
				return sortedSiblings[i].Run < sortedSiblings[j].Run
			})
			oldestChild := sortedSiblings[0]
			originalBranchID := fork.BodyHash + ":" + oldestChild.BodyHash
			if b.BranchID == originalBranchID {
				// b is the original sibling — not forked off anything
				// at THIS fork point. If fork has a parent (so fork
				// itself is mid-chain on a higher-level branch), the
				// branch containing fork's parent is the parent.
				if fork.ParentID != "" {
					if parent := byID[fork.ParentID]; parent != nil {
						b.ParentBranchID = parent.BranchID
					}
				}
			} else {
				b.ParentBranchID = originalBranchID
			}
		}
	}

	// Stable order: by CreatedAt ascending, then BranchID.
	sort.Slice(branches, func(i, j int) bool {
		if branches[i].CreatedAt != branches[j].CreatedAt {
			return branches[i].CreatedAt < branches[j].CreatedAt
		}
		return branches[i].BranchID < branches[j].BranchID
	})
	return branches
}

// pickDefaultBranchID implements §3.4: most recent activity wins,
// tie-break by node count desc, then branch_id lexicographic.
func pickDefaultBranchID(branches []*sessionBranch) string {
	if len(branches) == 0 {
		return ""
	}
	best := branches[0]
	for _, b := range branches[1:] {
		switch {
		case b.LastActivityAt > best.LastActivityAt:
			best = b
		case b.LastActivityAt < best.LastActivityAt:
			// keep
		case b.NodeCount > best.NodeCount:
			best = b
		case b.NodeCount < best.NodeCount:
			// keep
		case b.BranchID < best.BranchID:
			best = b
		}
	}
	return best.BranchID
}

// sessionDAGCache is an LRU-with-TTL cache of built DAGs keyed by
// session id. Built lazily; evicted on TTL expiry or LRU pressure
// (D9). Concurrency-safe.
type sessionDAGCache struct {
	mu      sync.Mutex
	entries map[string]*dagCacheEntry
	lru     *list.List // front = most recently used
	max     int
	ttl     time.Duration
}

type dagCacheEntry struct {
	dag      *sessionDAG
	lastUsed time.Time
	elem     *list.Element // entry's node in the LRU list
}

func newSessionDAGCache(max int, ttl time.Duration) *sessionDAGCache {
	return &sessionDAGCache{
		entries: make(map[string]*dagCacheEntry),
		lru:     list.New(),
		max:     max,
		ttl:     ttl,
	}
}

// Get returns the cached DAG for sid if present and fresh, else nil.
// Hits move the entry to LRU front.
func (c *sessionDAGCache) Get(sid string) *sessionDAG {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sid]
	if !ok {
		return nil
	}
	if time.Since(e.lastUsed) > c.ttl {
		c.lru.Remove(e.elem)
		delete(c.entries, sid)
		return nil
	}
	e.lastUsed = time.Now()
	c.lru.MoveToFront(e.elem)
	return e.dag
}

// Put stores dag for sid. Evicts least-recently-used entries when
// the cache is at capacity.
func (c *sessionDAGCache) Put(sid string, dag *sessionDAG) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[sid]; ok {
		existing.dag = dag
		existing.lastUsed = time.Now()
		c.lru.MoveToFront(existing.elem)
		return
	}
	for len(c.entries) >= c.max {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		oldSid := oldest.Value.(string)
		c.lru.Remove(oldest)
		delete(c.entries, oldSid)
	}
	elem := c.lru.PushFront(sid)
	c.entries[sid] = &dagCacheEntry{
		dag:      dag,
		lastUsed: time.Now(),
		elem:     elem,
	}
}

// Len returns the number of cached entries. Test-only helper.
func (c *sessionDAGCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// loadSessionRowsFromDirs scans every audit JSONL file in every dir
// and returns the rows belonging to sessionID — req rows that carry
// the session id as session_id, and resp rows paired with those req
// rows by (id, run).
//
// Resp rows do not carry session_id directly, so we collect req-side
// pair keys first, then include any resp row matching a known pair
// key. The returned slice is unsorted; buildSessionDAG handles
// chronological sort.
func loadSessionRowsFromDirs(dirs map[string]string, sessionID string) ([]json.RawMessage, error) {
	type pairKey struct {
		id  uint64
		run string
	}
	sessionPairs := make(map[pairKey]struct{})
	var reqRows []json.RawMessage
	respByKey := make(map[pairKey]json.RawMessage)

	for _, dir := range dirs {
		files, err := listJSONLByMTimeDesc(dir)
		if err != nil {
			// Best-effort across dirs — skip unreadable ones.
			continue
		}
		for _, e := range files {
			path := dir + "/" + e.Name()
			_ = scanFile(path, func(line []byte) bool {
				row, ok := parseAuditRow(line)
				if !ok {
					return true
				}
				key := pairKey{row.id, row.run}
				switch row.t {
				case "req":
					if row.sessionID == sessionID {
						sessionPairs[key] = struct{}{}
						reqRows = append(reqRows, append(json.RawMessage(nil), line...))
					}
				case "resp":
					respByKey[key] = append(json.RawMessage(nil), line...)
				}
				return true
			})
		}
	}

	out := make([]json.RawMessage, 0, len(reqRows)+len(sessionPairs))
	out = append(out, reqRows...)
	for key := range sessionPairs {
		if r, ok := respByKey[key]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}
