package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Golden-file "oracle anchor" tests for the translation layer.
//
// Existing a2o_test.go / o2a_test.go verify translator behavior field by
// field. That shape is good for regression coverage but weak against
// structural mistakes — a wrong role mapping, a renamed field, an extra
// unexpected key, or a dropped required attribute can all pass the
// field-by-field tests if the assertion set does not cover that field.
//
// These tests anchor the translation layer against hand-derived canonical
// JSON for both request and response directions in both orientations
// (a2o and o2a). The expected JSON is written by hand from the public
// Anthropic and OpenAI API docs, NOT computed via the translator itself,
// so a structural regression in the translator cannot quietly pass.
//
// Analogous to TestFingerprint_PinnedPEM for tlsutil (AP-1 anchor).

// normalizeJSON re-serializes in a stable order so hand-written and
// translator-produced byte sequences compare cleanly.
func normalizeJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v; input=%s", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return out
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	g := normalizeJSON(t, got)
	w := normalizeJSON(t, want)
	if !bytes.Equal(g, w) {
		t.Errorf("JSON mismatch.\n got: %s\nwant: %s", g, w)
	}
}

// TestOracle_A2O_Request anchors Anthropic→OpenAI request translation
// against hand-derived expected JSON. A structural regression in
// translateAnthropicRequest (wrong role, renamed field, missing
// stream_options on streaming, wrong tool shape) flips this red.
func TestOracle_A2O_Request(t *testing.T) {
	t.Parallel()

	// Input: a realistic Anthropic request covering system prompt,
	// user/assistant turns, a tool definition, temperature, and stop
	// sequences.
	input := []byte(`{
      "model": "claude-sonnet-4-6",
      "max_tokens": 1024,
      "system": "You are concise.",
      "temperature": 0.3,
      "stop_sequences": ["STOP", "END"],
      "messages": [
        {"role": "user", "content": "What is 2+2?"},
        {"role": "assistant", "content": "4"},
        {"role": "user", "content": "Why?"}
      ],
      "tools": [
        {
          "name": "calc",
          "description": "arithmetic",
          "input_schema": {"type": "object", "properties": {"x": {"type": "number"}}}
        }
      ]
    }`)

	// Hand-derived expected OpenAI request. Written from the OpenAI API
	// spec (https://platform.openai.com/docs/api-reference/chat/create),
	// NOT produced by the translator.
	//
	// Key structural commitments this pins:
	//   - system prompt becomes a {"role":"system"} message at the head
	//   - max_tokens is an integer (not string, not wrapped)
	//   - stop_sequences → stop as a JSON array
	//   - tools[i].function.parameters carries the raw input_schema
	//   - tool definitions are wrapped as {"type":"function","function":{...}}
	//   - strict is explicitly false (not omitted) on translated tools
	//   - temperature passes through as a float
	//   - no stream_options when stream is absent/false
	want := []byte(`{
      "model": "gpt-4o",
      "max_tokens": 1024,
      "stop": ["STOP", "END"],
      "temperature": 0.3,
      "messages": [
        {"role": "system", "content": "You are concise."},
        {"role": "user",   "content": "What is 2+2?"},
        {"role": "assistant", "content": "4"},
        {"role": "user",   "content": "Why?"}
      ],
      "tools": [
        {
          "type": "function",
          "function": {
            "name": "calc",
            "description": "arithmetic",
            "parameters": {"type": "object", "properties": {"x": {"type": "number"}}},
            "strict": false
          }
        }
      ]
    }`)

	var req MessagesRequest
	if err := json.Unmarshal(input, &req); err != nil {
		t.Fatal(err)
	}
	cfg := &UpstreamCfg{ModelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"}}
	out, err := translateAnthropicRequest(&req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, want)
}

// TestOracle_A2O_Request_Streaming anchors the stream_options side of
// the request translation: when stream=true, StreamOptions must be set
// to include_usage=true so usage rolls up in the final delta.
func TestOracle_A2O_Request_Streaming(t *testing.T) {
	t.Parallel()
	input := []byte(`{
      "model": "claude-sonnet-4-6",
      "max_tokens": 50,
      "stream": true,
      "messages": [{"role": "user", "content": "hi"}]
    }`)
	want := []byte(`{
      "model": "gpt-4o",
      "max_tokens": 50,
      "stream": true,
      "stream_options": {"include_usage": true},
      "messages": [{"role": "user", "content": "hi"}]
    }`)
	var req MessagesRequest
	if err := json.Unmarshal(input, &req); err != nil {
		t.Fatal(err)
	}
	cfg := &UpstreamCfg{ModelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"}}
	out, err := translateAnthropicRequest(&req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, want)
}

// TestOracle_A2O_Response anchors the OpenAI→Anthropic response
// translation (the return path of an a2o request). A structural
// regression in translateOpenAIResponse — wrong stop_reason mapping,
// missing usage, wrong content block shape, wrong role — flips red.
func TestOracle_A2O_Response(t *testing.T) {
	t.Parallel()

	// Input: a realistic OpenAI response with text content and usage.
	input := []byte(`{
      "id": "chatcmpl-abc",
      "object": "chat.completion",
      "created": 1704067200,
      "model": "gpt-4o",
      "choices": [
        {
          "index": 0,
          "message": {"role": "assistant", "content": "Hello there!"},
          "finish_reason": "stop"
        }
      ],
      "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
    }`)

	// Hand-derived expected Anthropic response. Pinned commitments:
	//   - id is prefixed with "msg_" to match Anthropic's convention
	//     (unconditional: prefix is always added, even if upstream id
	//     already had a different prefix — keeps behavior stateless)
	//   - type is "message", role is "assistant"
	//   - finish_reason "stop" maps to stop_reason "end_turn"
	//   - text content becomes a single ContentBlock with type="text"
	//   - model echoes the *client* model, not the upstream model
	//   - usage: prompt_tokens → input_tokens, completion_tokens → output_tokens
	want := []byte(`{
      "id": "msg_chatcmpl-abc",
      "type": "message",
      "role": "assistant",
      "model": "claude-sonnet-4-6",
      "content": [{"type": "text", "text": "Hello there!"}],
      "stop_reason": "end_turn",
      "usage": {"input_tokens": 12, "output_tokens": 3}
    }`)

	var resp ChatCompletionResponse
	if err := json.Unmarshal(input, &resp); err != nil {
		t.Fatal(err)
	}
	out, err := translateOpenAIResponse(&resp, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, want)
}

// TestOracle_O2A_Request anchors OpenAI→Anthropic request translation.
func TestOracle_O2A_Request(t *testing.T) {
	t.Parallel()

	// Input: a realistic OpenAI request with a system message and
	// a user message.
	input := []byte(`{
      "model": "gpt-4o",
      "max_tokens": 512,
      "temperature": 0.7,
      "messages": [
        {"role": "system", "content": "Be helpful."},
        {"role": "user", "content": "Hi"}
      ]
    }`)

	// Hand-derived expected Anthropic request. Pinned commitments:
	//   - OpenAI "system" messages are lifted out into the top-level system field (string form)
	//   - remaining user messages are canonicalized to content-block array form
	//     (spec permits both string and block-array; translator normalizes to blocks)
	//   - max_tokens is an integer
	//   - temperature passes through (≤1.0 so no clamping here)
	//   - model is mapped
	want := []byte(`{
      "model": "claude-sonnet-4-6",
      "max_tokens": 512,
      "temperature": 0.7,
      "system": "Be helpful.",
      "messages": [
        {"role": "user", "content": [{"type": "text", "text": "Hi"}]}
      ]
    }`)

	var req ChatCompletionRequest
	if err := json.Unmarshal(input, &req); err != nil {
		t.Fatal(err)
	}
	cfg := &UpstreamCfg{ModelMap: map[string]string{"gpt-4o": "claude-sonnet-4-6"}}
	out, err := translateOpenAIRequest(&req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, want)
}

// TestOracle_O2A_Response anchors Anthropic→OpenAI response translation
// (the return path of an o2a request).
func TestOracle_O2A_Response(t *testing.T) {
	t.Parallel()

	input := []byte(`{
      "id": "msg_xyz",
      "type": "message",
      "role": "assistant",
      "model": "claude-sonnet-4-6",
      "content": [{"type": "text", "text": "Sure."}],
      "stop_reason": "end_turn",
      "usage": {"input_tokens": 5, "output_tokens": 1}
    }`)

	// Hand-derived expected OpenAI response. Pinned commitments:
	//   - end_turn → stop
	//   - id is prefixed with "chatcmpl-" to match OpenAI's convention
	//     (unconditional: prefix is always added, even when upstream id
	//     already carries a different prefix — stateless mirror of the
	//     inverse A2O_Response behavior)
	//   - object is "chat.completion"
	//   - choices[0].index is 0
	//   - content lifted out into string message content
	//   - input_tokens → prompt_tokens, output_tokens → completion_tokens,
	//     total_tokens is their sum
	//   - model echoes the *client* model
	want := []byte(`{
      "id": "chatcmpl-msg_xyz",
      "object": "chat.completion",
      "created": 0,
      "model": "gpt-4o",
      "choices": [
        {
          "index": 0,
          "message": {"role": "assistant", "content": "Sure."},
          "finish_reason": "stop"
        }
      ],
      "usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6}
    }`)

	var resp MessagesResponse
	if err := json.Unmarshal(input, &resp); err != nil {
		t.Fatal(err)
	}
	out, err := translateAnthropicResponse(&resp, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	// `created` is wall-clock in the translator; strip it for
	// deterministic comparison but keep the rest.
	var g map[string]any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	g["created"] = float64(0)
	gotStable, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, gotStable, want)
}

// TestOracle_A2O_Request_ToolResult pins the tool-result message
// shape. Anthropic encodes tool results as a user-role message
// containing a tool_result content block; OpenAI encodes them as a
// tool-role message with tool_call_id.
func TestOracle_A2O_Request_ToolResult(t *testing.T) {
	t.Parallel()
	input := []byte(`{
      "model": "claude-sonnet-4-6",
      "max_tokens": 100,
      "messages": [
        {"role": "user", "content": [
          {"type": "tool_result", "tool_use_id": "toolu_123", "content": "42"}
        ]}
      ]
    }`)
	// Pinned: tool_result → role:"tool" with tool_call_id preserved unchanged.
	want := []byte(`{
      "model": "gpt-4o",
      "max_tokens": 100,
      "messages": [
        {"role": "tool", "content": "42", "tool_call_id": "toolu_123"}
      ]
    }`)
	var req MessagesRequest
	if err := json.Unmarshal(input, &req); err != nil {
		t.Fatal(err)
	}
	cfg := &UpstreamCfg{ModelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"}}
	out, err := translateAnthropicRequest(&req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, got, want)
}
