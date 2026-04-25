package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// reqRow builds a "req" audit row JSON-byte-string with the given id,
// run, ts, and body. Helper to keep the test data dense.
func reqRow(id uint64, run, ts string, body string) json.RawMessage {
	row := map[string]any{
		"t":    "req",
		"id":   id,
		"run":  run,
		"ts":   ts,
		"body": json.RawMessage(body),
	}
	out, err := json.Marshal(row)
	if err != nil {
		panic(err)
	}
	return out
}

// respRow builds a "resp" audit row paired to (id, run).
func respRow(id uint64, run, ts string, status int, outcome string, inTok, outTok int) json.RawMessage {
	row := map[string]any{
		"t":          "resp",
		"id":         id,
		"run":        run,
		"ts":         ts,
		"status":     status,
		"outcome":    outcome,
		"elapsed_ms": 100,
		"usage": map[string]any{
			"input_tokens":  inTok,
			"output_tokens": outTok,
		},
	}
	out, err := json.Marshal(row)
	if err != nil {
		panic(err)
	}
	return out
}

// chatBody emits a Claude-shaped JSON body with N user/assistant
// message turns. Each turn is two messages (user, assistant). The
// content for turn k is "u<k>" and "a<k>" so message arrays at
// different prefix lengths are visually distinct.
func chatBody(turns int, model string) string {
	msgs := make([]map[string]any, 0, turns*2)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("u%d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i)},
		)
	}
	body := map[string]any{
		"model":    model,
		"messages": msgs,
	}
	out, _ := json.Marshal(body)
	return string(out)
}

// chatBodyWithUserOnly emits a Claude-shaped JSON body where the
// final message is a user turn without the assistant response — the
// shape of an in-flight or re-roll request.
func chatBodyWithUserOnly(turns int, model string, finalUser string) string {
	msgs := make([]map[string]any, 0, turns*2+1)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("u%d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i)},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": finalUser})
	body := map[string]any{
		"model":    model,
		"messages": msgs,
	}
	out, _ := json.Marshal(body)
	return string(out)
}

// --- nodeIDFromBody / canonicalBodyHash ---

func TestNodeIDFromBody_Deterministic(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	first := nodeIDFromBody(body)
	second := nodeIDFromBody(body)
	if first != second {
		t.Errorf("nodeIDFromBody not deterministic: %s != %s", first, second)
	}
	if len(first) != nodeIDHexLen {
		t.Errorf("nodeIDFromBody length = %d, want %d", len(first), nodeIDHexLen)
	}
}

func TestNodeIDFromBody_DifferentBytesDifferentHash(t *testing.T) {
	t.Parallel()
	a := nodeIDFromBody([]byte(`{"a":1}`))
	b := nodeIDFromBody([]byte(`{"a":2}`))
	if a == b {
		t.Errorf("expected different hashes, got %s", a)
	}
}

func TestCanonicalBodyHash_KeyOrderInvariant(t *testing.T) {
	t.Parallel()
	a, errA := canonicalBodyHash([]byte(`{"model":"x","messages":[]}`))
	b, errB := canonicalBodyHash([]byte(`{"messages":[],"model":"x"}`))
	if errA != nil || errB != nil {
		t.Fatalf("errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("key-order changed canonical hash: %s != %s", a, b)
	}
}

func TestCanonicalBodyHash_WhitespaceInvariant(t *testing.T) {
	t.Parallel()
	a, _ := canonicalBodyHash([]byte(`{"model":"x","messages":[]}`))
	b, _ := canonicalBodyHash([]byte(`{
  "model": "x",
  "messages": []
}`))
	if a != b {
		t.Errorf("whitespace changed canonical hash: %s != %s", a, b)
	}
}

func TestCanonicalBodyHash_RejectsNonJSON(t *testing.T) {
	t.Parallel()
	_, err := canonicalBodyHash([]byte(`not-json`))
	if err == nil {
		t.Errorf("expected error on non-JSON input")
	}
}

// --- prefixCanonicalHashes ---

func TestPrefixCanonicalHashes_ProducesNPlus1Slots(t *testing.T) {
	t.Parallel()
	body := []byte(chatBody(3, "x"))
	hashes, count, err := prefixCanonicalHashes(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if count != 6 { // 3 turns × 2 messages
		t.Errorf("messageCount = %d, want 6", count)
	}
	if len(hashes) != 7 {
		t.Errorf("len(hashes) = %d, want 7", len(hashes))
	}
}

func TestPrefixCanonicalHashes_FullPrefixEqualsCanonicalBodyHash(t *testing.T) {
	t.Parallel()
	body := []byte(chatBody(2, "x"))
	hashes, count, _ := prefixCanonicalHashes(body)
	full, _ := canonicalBodyHash(body)
	if hashes[count] != full {
		t.Errorf("prefix at full length %d = %s, want canonical full = %s", count, hashes[count], full)
	}
}

func TestPrefixCanonicalHashes_DifferentModelsDifferentHashes(t *testing.T) {
	t.Parallel()
	hashesX, _, _ := prefixCanonicalHashes([]byte(chatBody(2, "x")))
	hashesY, _, _ := prefixCanonicalHashes([]byte(chatBody(2, "y")))
	// Same prefix length k=2, different model — must differ.
	if hashesX[2] == hashesY[2] {
		t.Errorf("model-only difference produced equal prefix hashes at k=2")
	}
}

// --- buildSessionDAG: linear extension ---

func TestBuildSessionDAG_LinearExtension(t *testing.T) {
	t.Parallel()
	// Three turns observed sequentially. Each request body extends
	// the previous by one (user, assistant) pair.
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
		respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
		reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBody(3, "x")),
		respRow(3, "r1", "2026-04-25T10:00:05Z", 200, "ok", 30, 5),
	}
	dag, err := buildSessionDAG("s1", rows)
	if err != nil {
		t.Fatalf("buildSessionDAG: %v", err)
	}
	if len(dag.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(dag.Nodes))
	}
	if dag.Nodes[0].ParentID != "" {
		t.Errorf("node 0 parent = %q, want empty (root)", dag.Nodes[0].ParentID)
	}
	if dag.Nodes[1].ParentID != dag.Nodes[0].BodyHash {
		t.Errorf("node 1 parent = %q, want %q", dag.Nodes[1].ParentID, dag.Nodes[0].BodyHash)
	}
	if dag.Nodes[2].ParentID != dag.Nodes[1].BodyHash {
		t.Errorf("node 2 parent = %q, want %q", dag.Nodes[2].ParentID, dag.Nodes[1].BodyHash)
	}
	// One branch with one leaf — the latest node.
	if len(dag.Branches) != 1 {
		t.Fatalf("branches = %d, want 1", len(dag.Branches))
	}
	if dag.Branches[0].TipID != dag.Nodes[2].BodyHash {
		t.Errorf("tip = %q, want %q", dag.Branches[0].TipID, dag.Nodes[2].BodyHash)
	}
	if dag.Branches[0].NodeCount != 3 {
		t.Errorf("branch node_count = %d, want 3", dag.Branches[0].NodeCount)
	}
	// All three nodes share the same branch_id.
	if dag.Nodes[0].BranchID != dag.Nodes[2].BranchID {
		t.Errorf("linear-chain nodes do not share branch_id: %q vs %q", dag.Nodes[0].BranchID, dag.Nodes[2].BranchID)
	}
	// Default branch is the only branch.
	if dag.DefaultBranchID != dag.Branches[0].BranchID {
		t.Errorf("default_branch_id = %q, want %q", dag.DefaultBranchID, dag.Branches[0].BranchID)
	}
}

// --- buildSessionDAG: response summary fields ---

func TestBuildSessionDAG_ResponseSummaryPopulated(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 12, 7),
	}
	dag, _ := buildSessionDAG("s1", rows)
	rs := dag.Nodes[0].ResponseSummary
	if !rs.HasResponse {
		t.Errorf("HasResponse = false, want true")
	}
	if rs.Status != 200 || rs.Outcome != "ok" || rs.InputTokens != 12 || rs.OutputTokens != 7 {
		t.Errorf("response summary = %+v", rs)
	}
}

func TestBuildSessionDAG_OrphanWhenRespMissing(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
	}
	dag, _ := buildSessionDAG("s1", rows)
	if dag.Nodes[0].ResponseSummary.HasResponse {
		t.Errorf("HasResponse = true with no resp row")
	}
}

// --- buildSessionDAG: fork ---

func TestBuildSessionDAG_Fork(t *testing.T) {
	t.Parallel()
	// N1 (turn 1) — N2 (turn 2). Then N3 forks from N1 with a
	// different turn 2 user message ("alt"). N3's parent is N1.
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
		respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
		// N3 forks from N1 with an alternate continuation. Its
		// messages are [u0, a0, alt-user] so the prefix at k=2 is
		// canonical-equal to N1's full body. Prefix-match finds
		// N1 as parent (D4).
		reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBodyWithUserOnly(1, "x", "alt")),
		respRow(3, "r1", "2026-04-25T10:00:05Z", 200, "ok", 25, 5),
	}
	dag, err := buildSessionDAG("s1", rows)
	if err != nil {
		t.Fatalf("buildSessionDAG: %v", err)
	}
	if len(dag.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(dag.Nodes))
	}
	n1, n2, n3 := dag.Nodes[0], dag.Nodes[1], dag.Nodes[2]
	if n2.ParentID != n1.BodyHash {
		t.Errorf("n2 parent = %q, want n1 = %q", n2.ParentID, n1.BodyHash)
	}
	if n3.ParentID != n1.BodyHash {
		t.Errorf("n3 parent = %q, want n1 = %q (fork)", n3.ParentID, n1.BodyHash)
	}
	// Two branches — one tipped at n2, one at n3.
	if len(dag.Branches) != 2 {
		t.Fatalf("branches = %d, want 2; dag=%+v", len(dag.Branches), dag.Branches)
	}
	tips := map[string]bool{}
	for _, b := range dag.Branches {
		tips[b.TipID] = true
	}
	if !tips[n2.BodyHash] || !tips[n3.BodyHash] {
		t.Errorf("expected tips at n2 and n3, got %+v", tips)
	}
	// n2 and n3 must have different branch_ids.
	if n2.BranchID == n3.BranchID {
		t.Errorf("siblings share branch_id %q", n2.BranchID)
	}
	// Both branch_ids start with id(n1) (the fork point).
	for _, b := range dag.Branches {
		if !strings.HasPrefix(b.BranchID, n1.BodyHash+":") {
			t.Errorf("branch_id %q does not have fork-point prefix %q", b.BranchID, n1.BodyHash)
		}
	}
	// Default = most recent activity = n3 branch.
	for _, b := range dag.Branches {
		if b.TipID == n3.BodyHash && dag.DefaultBranchID != b.BranchID {
			t.Errorf("default_branch_id = %q, want %q (n3 branch, more recent)", dag.DefaultBranchID, b.BranchID)
		}
	}
}

// --- buildSessionDAG: re-roll (newest-match-wins) ---

func TestBuildSessionDAG_ReRollNewestWins(t *testing.T) {
	t.Parallel()
	// Same canonical state appears twice (e.g. silent retry). The
	// next request that prefix-matches should take the most recent
	// occurrence as parent (D5).
	bodyA := chatBody(1, "x")
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", bodyA),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		// Identical body re-sent.
		reqRow(2, "r1", "2026-04-25T10:00:02Z", bodyA),
		respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 10, 5),
		// Extension: next turn. Should pick the most recent
		// matching prior (id=2), not the original (id=1).
		reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBody(2, "x")),
	}
	dag, _ := buildSessionDAG("s1", rows)
	// Node 1 and Node 2 have identical bodies → identical
	// BodyHash. The DAG keeps both as separate entries (they are
	// separate audit rows and may have different responses).
	if dag.Nodes[0].BodyHash != dag.Nodes[1].BodyHash {
		t.Fatalf("identical bodies hashed differently: %q vs %q", dag.Nodes[0].BodyHash, dag.Nodes[1].BodyHash)
	}
	// Node 3's parent is the most recent prior with matching
	// canonical state — that's Node 2 (newest match wins).
	if dag.Nodes[2].ParentID != dag.Nodes[1].BodyHash {
		t.Errorf("node 3 parent = %q, want node 2 (newest match) = %q", dag.Nodes[2].ParentID, dag.Nodes[1].BodyHash)
	}
}

// --- buildSessionDAG: /clear (orphan root) ---

func TestBuildSessionDAG_ClearProducesOrphanRoot(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
		respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
		// /clear: brand-new conversation, no prefix match anywhere.
		// Different model name to make sure no accidental match.
		reqRow(3, "r1", "2026-04-25T10:01:00Z", `{"model":"y","messages":[{"role":"user","content":"fresh"}]}`),
		respRow(3, "r1", "2026-04-25T10:01:01Z", 200, "ok", 5, 3),
	}
	dag, _ := buildSessionDAG("s1", rows)
	if dag.Nodes[2].ParentID != "" {
		t.Errorf("post-clear node parent = %q, want empty (orphan root)", dag.Nodes[2].ParentID)
	}
	// Two branches: the original and the post-clear branch.
	if len(dag.Branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(dag.Branches))
	}
}

// --- buildSessionDAG: malformed body ---

func TestBuildSessionDAG_MalformedBodyOrphan(t *testing.T) {
	t.Parallel()
	// A non-JSON body becomes an orphan node — no prefix matching
	// possible. The wire-bytes BodyHash still works (V1 fallback).
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", `"not-an-object"`),
	}
	dag, _ := buildSessionDAG("s1", rows)
	if dag.Nodes[1].ParentID != "" {
		t.Errorf("malformed-body node parent = %q, want empty", dag.Nodes[1].ParentID)
	}
}

// --- buildSessionDAG: multi-message extension (skip a turn) ---

func TestBuildSessionDAG_MultiMessageExtension(t *testing.T) {
	t.Parallel()
	// N1 has 2 messages [u0, a0]. N2 has 6 messages — extending
	// N1 by 4 messages (rare but real if a tool turn is rolled in
	// to the same request). Last-message-only would reject;
	// prefix-match finds N1 as parent (§2.3).
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(3, "x")),
	}
	dag, _ := buildSessionDAG("s1", rows)
	if dag.Nodes[1].ParentID != dag.Nodes[0].BodyHash {
		t.Errorf("multi-message extension parent = %q, want %q", dag.Nodes[1].ParentID, dag.Nodes[0].BodyHash)
	}
}

// --- branch derivation: parent_branch_id ---

func TestBuildSessionDAG_ForkedBranchParentBranchID(t *testing.T) {
	t.Parallel()
	// Tree:  N1 → N2 → N3 (branch A, leaf N3)
	//        N1 → N4 (branch B, leaf N4)
	// branch B's root is N4 (the first node after the fork point N1).
	// branch B's parent_branch_id is the branch that contains N1,
	// which is branch A (since N1 is on the chain to N3).
	rows := []json.RawMessage{
		reqRow(1, "r1", "2026-04-25T10:00:00Z", chatBody(1, "x")),
		respRow(1, "r1", "2026-04-25T10:00:01Z", 200, "ok", 10, 5),
		reqRow(2, "r1", "2026-04-25T10:00:02Z", chatBody(2, "x")),
		respRow(2, "r1", "2026-04-25T10:00:03Z", 200, "ok", 20, 5),
		reqRow(3, "r1", "2026-04-25T10:00:04Z", chatBody(3, "x")),
		respRow(3, "r1", "2026-04-25T10:00:05Z", 200, "ok", 30, 5),
		// Fork from N1 with alt content.
		reqRow(4, "r1", "2026-04-25T10:00:06Z", chatBodyWithUserOnly(1, "x", "alt")),
	}
	dag, _ := buildSessionDAG("s1", rows)
	if len(dag.Branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(dag.Branches))
	}
	for _, b := range dag.Branches {
		if b.TipID == dag.Nodes[3].BodyHash { // tip = N4 → branch B
			if b.ParentBranchID == "" {
				t.Errorf("forked branch parent_branch_id is empty")
			}
		}
	}
}

// --- sessionDAGCache ---

func TestSessionDAGCache_GetMissing(t *testing.T) {
	t.Parallel()
	c := newSessionDAGCache(4, time.Minute)
	if c.Get("nope") != nil {
		t.Errorf("expected nil for missing key")
	}
}

func TestSessionDAGCache_PutAndGet(t *testing.T) {
	t.Parallel()
	c := newSessionDAGCache(4, time.Minute)
	dag := &sessionDAG{SessionID: "s1"}
	c.Put("s1", dag)
	if got := c.Get("s1"); got != dag {
		t.Errorf("cache miss: got %v want %v", got, dag)
	}
}

func TestSessionDAGCache_TTLExpires(t *testing.T) {
	t.Parallel()
	c := newSessionDAGCache(4, 10*time.Millisecond)
	c.Put("s1", &sessionDAG{SessionID: "s1"})
	// Manually expire by rewriting lastUsed in the past.
	c.mu.Lock()
	c.entries["s1"].lastUsed = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	if c.Get("s1") != nil {
		t.Errorf("expected TTL eviction")
	}
	if c.Len() != 0 {
		t.Errorf("expected empty cache after TTL eviction, len=%d", c.Len())
	}
}

func TestSessionDAGCache_LRUEviction(t *testing.T) {
	t.Parallel()
	c := newSessionDAGCache(2, time.Minute)
	c.Put("a", &sessionDAG{SessionID: "a"})
	c.Put("b", &sessionDAG{SessionID: "b"})
	// Touch "a" so "b" becomes LRU.
	if c.Get("a") == nil {
		t.Fatalf("a missing")
	}
	c.Put("c", &sessionDAG{SessionID: "c"})
	if c.Get("b") != nil {
		t.Errorf("b should have been evicted as LRU")
	}
	if c.Get("a") == nil || c.Get("c") == nil {
		t.Errorf("a and c should remain in cache")
	}
}

func TestSessionDAGCache_PutOverwrites(t *testing.T) {
	t.Parallel()
	c := newSessionDAGCache(4, time.Minute)
	d1 := &sessionDAG{SessionID: "s"}
	d2 := &sessionDAG{SessionID: "s"}
	c.Put("s", d1)
	c.Put("s", d2)
	if got := c.Get("s"); got != d2 {
		t.Errorf("overwrite did not take effect")
	}
	if c.Len() != 1 {
		t.Errorf("len = %d, want 1", c.Len())
	}
}
