package gateway

import (
	"net/http"
	"testing"
)

// TestExtractSessionInfo_StableAcrossTurns is the load-bearing assertion of
// the whole session feature: two requests from the same conversation must
// produce the same session id, even though every subsequent turn appends new
// messages. The hash is over messages[0] specifically because that element
// is the byte-stable bootstrap of the chat.
func TestExtractSessionInfo_StableAcrossTurns(t *testing.T) {
	t.Parallel()
	turn1 := []byte(`{"model":"claude-opus-4-6","messages":[
		{"role":"user","content":"What is the capital of France?"}
	]}`)
	turn2 := []byte(`{"model":"claude-opus-4-6","messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."},
		{"role":"user","content":"And of Germany?"}
	]}`)
	turn3 := []byte(`{"model":"claude-opus-4-6","messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."},
		{"role":"user","content":"And of Germany?"},
		{"role":"assistant","content":"Berlin."},
		{"role":"user","content":"And of Spain?"}
	]}`)

	id1, n1 := extractSessionInfo(http.Header{}, turn1)
	id2, n2 := extractSessionInfo(http.Header{}, turn2)
	id3, n3 := extractSessionInfo(http.Header{}, turn3)

	if id1 == "" {
		t.Fatal("session id empty for turn1")
	}
	if id1 != id2 || id2 != id3 {
		t.Errorf("session id drifted across turns: %q, %q, %q", id1, id2, id3)
	}
	if len(id1) != sessionIDLen {
		t.Errorf("session id length = %d, want %d", len(id1), sessionIDLen)
	}
	if n1 != 1 || n2 != 3 || n3 != 5 {
		t.Errorf("turn counts = %d, %d, %d; want 1, 3, 5", n1, n2, n3)
	}
}

// TestExtractSessionInfo_DistinctConversations confirms that two chats with
// different opening messages produce distinct ids — the whole point of
// hashing instead of using a constant.
func TestExtractSessionInfo_DistinctConversations(t *testing.T) {
	t.Parallel()
	a := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	b := []byte(`{"messages":[{"role":"user","content":"goodbye"}]}`)
	idA, _ := extractSessionInfo(http.Header{}, a)
	idB, _ := extractSessionInfo(http.Header{}, b)
	if idA == "" || idB == "" {
		t.Fatalf("ids missing: %q %q", idA, idB)
	}
	if idA == idB {
		t.Errorf("distinct conversations collided: %q", idA)
	}
}

// TestExtractSessionInfo_HeaderOverride verifies that an explicit
// X-Mesh-Session header always wins, including when the body has no messages
// array at all (so the fallback hash would have produced "").
func TestExtractSessionInfo_HeaderOverride(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set(sessionHeader, "my-stable-id")

	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	id, turns := extractSessionInfo(h, body)
	if id != "my-stable-id" {
		t.Errorf("header override ignored, got %q", id)
	}
	if turns != 1 {
		t.Errorf("turn index lost when header set: %d", turns)
	}

	// Even with no messages array, the header still wins.
	id2, turns2 := extractSessionInfo(h, []byte(`{"model":"x"}`))
	if id2 != "my-stable-id" {
		t.Errorf("header lost on empty messages, got %q", id2)
	}
	if turns2 != 0 {
		t.Errorf("expected 0 turns with no messages, got %d", turns2)
	}
}

// TestExtractSessionInfo_HeaderCapped guards against a hostile/buggy client
// stuffing megabytes into the override header.
func TestExtractSessionInfo_HeaderCapped(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'x'
	}
	h.Set(sessionHeader, string(long))
	id, _ := extractSessionInfo(h, nil)
	if len(id) != 64 {
		t.Errorf("header not capped, len=%d", len(id))
	}
}

// TestExtractSessionInfo_MissingOrInvalid covers every case where audit must
// gracefully omit the field instead of fabricating one.
func TestExtractSessionInfo_MissingOrInvalid(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty body":     nil,
		"non-json body":  []byte("not json at all"),
		"no messages":    []byte(`{"model":"x"}`),
		"empty messages": []byte(`{"messages":[]}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, turns := extractSessionInfo(http.Header{}, body)
			if id != "" || turns != 0 {
				t.Errorf("%s: got id=%q turns=%d, want empty", name, id, turns)
			}
		})
	}
}
