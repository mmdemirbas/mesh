package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Header names checked by extractSessionInfo, in resolution order.
// See §4.4 of SPEC_PHASE1A.local.md for the contract: explicit
// override first, then the client-asserted ids most LLM tools send,
// then a content-derived fallback.
const (
	// sessionHeader (X-Mesh-Session) is the explicit override —
	// clients that already have a stable session id (a wrapper, a
	// scripted harness) can opt out of the heuristics.
	sessionHeader = "X-Mesh-Session"
	// claudeCodeSessionHeader is what Claude Code sends today.
	// Verified live (2026-04-17 audit log: 266 turns under one id).
	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
	// anthropicConversationHeader is a defensive forward-look for
	// hosted Anthropic clients that may carry conversation
	// identifiers; not seen in current traffic but cheap to honor.
	anthropicConversationHeader = "Anthropic-Conversation-Id"
)

// sessionIDLen caps the hex-encoded session id length. 12 hex chars = 48 bits
// of entropy from SHA-256, which is plenty for grouping rows in a single
// audit log; full digests would just bloat the JSONL.
const sessionIDLen = 12

// extractSessionInfo derives a session id and turn index from a request.
//
// Resolution order (§4.4):
//  1. X-Mesh-Session header (explicit override).
//  2. X-Claude-Code-Session-Id header (Claude Code's native id).
//  3. Anthropic-Conversation-Id header (defensive forward-look).
//  4. SHA-256(messages[0]) prefix — content-derived fallback.
//     Fragments on auto-compact and /clear; only used by clients
//     that send none of the above.
//
// The turn index is the count of messages in the request — turn 1 has one
// message, turn N has 2N-1 (alternating user/assistant) plus the new user
// message. Stored verbatim so the UI can show prompt-growth deltas.
//
// Returns ("", 0) when neither a header nor a parseable body yields
// a usable id; the audit row will simply omit the fields.
func extractSessionInfo(headers http.Header, body []byte) (string, int) {
	for _, name := range []string{sessionHeader, claudeCodeSessionHeader, anthropicConversationHeader} {
		h := strings.TrimSpace(headers.Get(name))
		if h == "" {
			continue
		}
		// Cap header value length so a hostile client can't pad the
		// id to balloon audit row size.
		if len(h) > 64 {
			h = h[:64]
		}
		return h, peekMessageCount(body)
	}
	if len(body) == 0 || !json.Valid(body) {
		return "", 0
	}
	var peek struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &peek); err != nil || len(peek.Messages) == 0 {
		return "", 0
	}
	sum := sha256.Sum256(peek.Messages[0])
	return hex.EncodeToString(sum[:])[:sessionIDLen], len(peek.Messages)
}

// peekMessageCount returns len(messages) without computing a hash. Used when
// the header already supplied an id but we still want a turn index.
func peekMessageCount(body []byte) int {
	if len(body) == 0 || !json.Valid(body) {
		return 0
	}
	var peek struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return 0
	}
	return len(peek.Messages)
}
