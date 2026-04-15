package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// sessionHeader, when present on a request, overrides the derived session id.
// Lets clients that already have a stable session identifier (a wrapper, a
// scripted harness) opt out of the heuristic.
const sessionHeader = "X-Mesh-Session"

// sessionIDLen caps the hex-encoded session id length. 12 hex chars = 48 bits
// of entropy from SHA-256, which is plenty for grouping rows in a single
// audit log; full digests would just bloat the JSONL.
const sessionIDLen = 12

// extractSessionInfo derives a session id and turn index from a request body.
//
// The session id is a 12-char prefix of SHA-256(messages[0]), where the first
// message is the byte-stable bootstrap of the conversation (Claude Code and
// most LLM clients replay full history on every turn, so messages[0] is
// identical across turns of the same chat). When the request carries an
// explicit X-Mesh-Session header that value wins.
//
// The turn index is the count of messages in the request — turn 1 has one
// message, turn N has 2N-1 (alternating user/assistant) plus the new user
// message. Stored verbatim so the UI can show prompt-growth deltas.
//
// Returns ("", 0) when the body is not parseable JSON or has no messages
// array; the audit row will simply omit the fields.
func extractSessionInfo(headers http.Header, body []byte) (string, int) {
	if h := strings.TrimSpace(headers.Get(sessionHeader)); h != "" {
		// Already opaque from the client; pass through capped.
		if len(h) > 64 {
			h = h[:64]
		}
		// Still try to count messages for turn_index even when the id is
		// supplied; a missing count is harmless but a present one is useful.
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
