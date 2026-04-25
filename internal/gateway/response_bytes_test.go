package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDeriveStreamTerminated_DecisionTree pins the §4.3 race
// semantics: terminal-marker-seen wins over context cancellation
// (handles the "client closes after message_stop but before final
// flush" race the reviewer specifically flagged), and only the
// "upstream + ctx-cancelled" combination flips to "client".
func TestDeriveStreamTerminated_DecisionTree(t *testing.T) {
	t.Parallel()
	ctxErr := context.Canceled
	cases := []struct {
		name    string
		reassem string
		ctxErr  error
		want    string
	}{
		{"normal + no cancel → normal", "normal", nil, "normal"},
		{"normal + cancel → still normal (race resolution)", "normal", ctxErr, "normal"},
		{"upstream + no cancel → upstream", "upstream", nil, "upstream"},
		{"upstream + cancel → client (the only flip)", "upstream", ctxErr, "client"},
		{"unknown reassembler value passes through", "weird", ctxErr, "weird"},
		{"upstream + DeadlineExceeded → still flips to client",
			"upstream", context.DeadlineExceeded, "client"},
		{"upstream + bare error → still flips (any non-nil ctxErr counts)",
			"upstream", errors.New("oops"), "client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveStreamTerminated(c.reassem, c.ctxErr); got != c.want {
				t.Errorf("got %q, want %q (reassem=%q ctxErr=%v)", got, c.want, c.reassem, c.ctxErr)
			}
		})
	}
}

// --- I3: streaming partition closure ---

// TestResponseBytes_AnthropicStreamingPartitionClosure pins the §4.3
// I3 invariant: for every captured Anthropic SSE stream,
// ResponseByteCounts.Total == len(decoded body) AND the named
// sections + Other == Total.
func TestResponseBytes_AnthropicStreamingPartitionClosure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "text only",
			body: []byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus","usage":{"input_tokens":5,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`),
		},
		{
			name: "thinking + text",
			body: []byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning here"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

event: message_stop
data: {"type":"message_stop"}

`),
		},
		{
			name: "tool use",
			body: []byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"Read"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/foo\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := reassembleSSE(c.body, APIAnthropic)
			if s.ResponseBytes == nil {
				t.Fatal("ResponseBytes nil")
			}
			rb := s.ResponseBytes
			if rb.Total != len(c.body) {
				t.Errorf("Total = %d, want %d (len(body))", rb.Total, len(c.body))
			}
			if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
				t.Errorf("partition broken: text=%d thinking=%d tool_calls=%d other=%d sum=%d total=%d",
					rb.Text, rb.Thinking, rb.ToolCalls, rb.Other, sum, rb.Total)
			}
			if rb.Other < 0 {
				t.Errorf("Other negative: %d (named section overcounted)", rb.Other)
			}
		})
	}
}

// TestResponseBytes_OpenAIStreamingPartitionClosure mirrors the
// Anthropic test for OpenAI chunks. Includes a panshi-shape case
// with delta.reasoning_content to pin the thinking-bucket mapping.
func TestResponseBytes_OpenAIStreamingPartitionClosure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "text only",
			body: []byte(`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"delta":{"content":" there"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`),
		},
		{
			name: "panshi reasoning_content lands in thinking bucket",
			body: []byte(`data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"reasoning_content":"thinking step 1"},"finish_reason":null}]}

data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"reasoning_content":" step 2"},"finish_reason":null}]}

data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"content":"final answer"},"finish_reason":null}]}

data: {"id":"x","model":"glm-4.7","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`),
		},
		{
			name: "tool calls",
			body: []byte(`data: {"id":"x","model":"gpt-4o","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":""}}]},"finish_reason":null}]}

data: {"id":"x","model":"gpt-4o","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"file_path\":\"/foo\"}"}}]},"finish_reason":null}]}

data: {"id":"x","model":"gpt-4o","choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := reassembleSSE(c.body, APIOpenAI)
			if s.ResponseBytes == nil {
				t.Fatal("ResponseBytes nil")
			}
			rb := s.ResponseBytes
			if rb.Total != len(c.body) {
				t.Errorf("Total = %d, want %d (len(body))", rb.Total, len(c.body))
			}
			if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
				t.Errorf("partition broken: text=%d thinking=%d tool_calls=%d other=%d sum=%d total=%d",
					rb.Text, rb.Thinking, rb.ToolCalls, rb.Other, sum, rb.Total)
			}
		})
	}
}

// TestResponseBytes_PanshiReasoningContentPopulatesThinking pins the
// reviewer-confirmed mapping: delta.reasoning_content (panshi /
// DeepSeek R1 / Qwen QwQ) lands in ResponseBytes.Thinking, NOT
// Text or ToolCalls. If a future change accidentally moves it, this
// test fails with a specific message.
func TestResponseBytes_PanshiReasoningContentPopulatesThinking(t *testing.T) {
	t.Parallel()
	body := []byte(`data: {"id":"x","model":"glm-4.7","choices":[{"delta":{"reasoning_content":"abcde"},"finish_reason":null}]}

data: [DONE]

`)
	s := reassembleSSE(body, APIOpenAI)
	if s.ResponseBytes == nil {
		t.Fatal("ResponseBytes nil")
	}
	if s.ResponseBytes.Thinking != 5 {
		t.Errorf("Thinking = %d, want 5 (panshi reasoning_content payload length)", s.ResponseBytes.Thinking)
	}
	if s.ResponseBytes.Text != 0 {
		t.Errorf("Text = %d, want 0 (reasoning_content must NOT leak into text)", s.ResponseBytes.Text)
	}
	if s.Thinking != "abcde" {
		t.Errorf("SSESummary.Thinking = %q, want %q", s.Thinking, "abcde")
	}
}

// --- I4: non-streaming partition closure ---

// TestResponseBytes_AnthropicNonStreamingPartitionClosure: same I2
// invariant for the non-streaming JSON response shape.
func TestResponseBytes_AnthropicNonStreamingPartitionClosure(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[` +
		`{"type":"text","text":"hello world"},` +
		`{"type":"thinking","thinking":"reasoning"},` +
		`{"type":"tool_use","id":"toolu_x","name":"Read","input":{"file_path":"/foo"}}` +
		`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":12}}`)

	rb := parseResponseBytes(body, APIAnthropic)
	if rb == nil {
		t.Fatal("parseResponseBytes returned nil for non-empty body")
	}
	if rb.Total != len(body) {
		t.Errorf("Total = %d, want %d", rb.Total, len(body))
	}
	if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
		t.Errorf("partition broken: %d != %d", sum, rb.Total)
	}
	if rb.Text != len("hello world") {
		t.Errorf("Text = %d, want %d", rb.Text, len("hello world"))
	}
	if rb.Thinking != len("reasoning") {
		t.Errorf("Thinking = %d, want %d", rb.Thinking, len("reasoning"))
	}
}

// TestResponseBytes_OpenAINonStreamingPartitionClosure: non-streaming
// OpenAI shape including reasoning_content + tool_calls.
func TestResponseBytes_OpenAINonStreamingPartitionClosure(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"the answer","reasoning_content":"reasoning",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/foo\"}"}}]},"finish_reason":"tool_calls"}` +
		`],"usage":{"prompt_tokens":5,"completion_tokens":10}}`)

	rb := parseResponseBytes(body, APIOpenAI)
	if rb == nil {
		t.Fatal("nil for non-empty body")
	}
	if rb.Total != len(body) {
		t.Errorf("Total = %d, want %d", rb.Total, len(body))
	}
	if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
		t.Errorf("partition broken: %d != %d", sum, rb.Total)
	}
	if rb.Text != len("the answer") {
		t.Errorf("Text = %d, want %d", rb.Text, len("the answer"))
	}
	if rb.Thinking != len("reasoning") {
		t.Errorf("Thinking = %d, want %d (OpenAI non-streaming reasoning_content → thinking)", rb.Thinking, len("reasoning"))
	}
}

// TestResponseBytes_NonStreamingMalformedAllOther: non-JSON body
// produces all-Other partition. Same graceful-degrade pattern as
// SectionBytes on the request side.
func TestResponseBytes_NonStreamingMalformedAllOther(t *testing.T) {
	t.Parallel()
	body := []byte(`not json at all`)
	rb := parseResponseBytes(body, APIAnthropic)
	if rb == nil {
		t.Fatal("nil for non-empty malformed body")
	}
	if rb.Other != len(body) {
		t.Errorf("Other = %d, want %d (malformed → all to Other)", rb.Other, len(body))
	}
	if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
		t.Errorf("partition broken on malformed: %d != %d", sum, rb.Total)
	}
}

// TestResponseBytes_NonStreamingEmptyReturnsNil: empty body short-
// circuits to nil. The audit row presence rule then omits the
// response_bytes field, mirroring how parseUsage handles empty
// bodies.
func TestResponseBytes_NonStreamingEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if got := parseResponseBytes(nil, APIAnthropic); got != nil {
		t.Errorf("nil body should return nil, got %+v", got)
	}
	if got := parseResponseBytes([]byte{}, APIOpenAI); got != nil {
		t.Errorf("empty body should return nil, got %+v", got)
	}
}

// --- §4.3 stream.terminated decision tree ---

// TestStreamTerminated_AnthropicMessageStopYieldsNormal: terminal
// marker present → "normal".
func TestStreamTerminated_AnthropicMessageStopYieldsNormal(t *testing.T) {
	t.Parallel()
	body := []byte(`event: message_stop
data: {"type":"message_stop"}

`)
	s := reassembleSSE(body, APIAnthropic)
	if s.Terminated != "normal" {
		t.Errorf("Terminated = %q, want normal", s.Terminated)
	}
}

// TestStreamTerminated_AnthropicNoTerminalYieldsUpstream: stream
// ended without message_stop → reassembler reports "upstream".
// (wrapAuditing may upgrade to "client" if context cancellation
// took precedence — see TestStreamTerminated_ClientCancellationFlip.)
func TestStreamTerminated_AnthropicNoTerminalYieldsUpstream(t *testing.T) {
	t.Parallel()
	body := []byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"oops cut off"}}

`)
	s := reassembleSSE(body, APIAnthropic)
	if s.Terminated != "upstream" {
		t.Errorf("Terminated = %q, want upstream (no message_stop seen)", s.Terminated)
	}
}

// TestStreamTerminated_OpenAIDoneMarkerYieldsNormal: [DONE] marker
// is the OpenAI equivalent of message_stop.
func TestStreamTerminated_OpenAIDoneMarkerYieldsNormal(t *testing.T) {
	t.Parallel()
	body := []byte(`data: {"id":"x","model":"gpt-4o","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: [DONE]

`)
	s := reassembleSSE(body, APIOpenAI)
	if s.Terminated != "normal" {
		t.Errorf("Terminated = %q, want normal", s.Terminated)
	}
}

// TestStreamTerminated_OpenAINoDoneYieldsUpstream: stream lacks
// the [DONE] marker → "upstream".
func TestStreamTerminated_OpenAINoDoneYieldsUpstream(t *testing.T) {
	t.Parallel()
	body := []byte(`data: {"id":"x","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}]}

`)
	s := reassembleSSE(body, APIOpenAI)
	if s.Terminated != "upstream" {
		t.Errorf("Terminated = %q, want upstream", s.Terminated)
	}
}

// TestStreamTerminated_ErrorEventStillUpstream: an Anthropic
// `event: error` mid-stream does NOT count as a terminal marker.
// Spec: status field disambiguates; terminated stays "upstream".
func TestStreamTerminated_ErrorEventStillUpstream(t *testing.T) {
	t.Parallel()
	body := []byte(`event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"backend overloaded"}}

`)
	s := reassembleSSE(body, APIAnthropic)
	if s.Terminated != "upstream" {
		t.Errorf("Terminated = %q, want upstream (error events do not terminate normally)", s.Terminated)
	}
	if len(s.Errors) != 1 || !strings.Contains(s.Errors[0], "overloaded") {
		t.Errorf("error event not captured in summary.Errors: %+v", s.Errors)
	}
}

// --- empty/degenerate streams (the "easy to forget" cases) ---

// TestResponseBytes_EmptyAnthropicStreamPartitionClosesTrivially:
// reviewer-flagged degenerate case. Upstream connects, sends
// nothing, closes. Partition closes (Total=0=Other), terminator
// is "upstream".
func TestResponseBytes_EmptyAnthropicStreamPartitionClosesTrivially(t *testing.T) {
	t.Parallel()
	s := reassembleSSE(nil, APIAnthropic)
	if s == nil {
		t.Fatal("nil summary for empty Anthropic stream — partition cannot close")
	}
	if s.ResponseBytes == nil {
		t.Fatal("ResponseBytes nil for empty stream")
	}
	if s.ResponseBytes.Total != 0 || s.ResponseBytes.Other != 0 {
		t.Errorf("empty: Total=%d Other=%d, want 0,0", s.ResponseBytes.Total, s.ResponseBytes.Other)
	}
	if s.Events != 0 {
		t.Errorf("empty Events = %d, want 0", s.Events)
	}
	if s.Terminated != "upstream" {
		t.Errorf("empty Terminated = %q, want upstream", s.Terminated)
	}
}

// TestResponseBytes_OnlyDoneMarkerStreamAllOther: reviewer-flagged
// case from the other side. Stream contains nothing but [DONE].
// All bytes go to Other; Terminated="normal".
func TestResponseBytes_OnlyDoneMarkerStreamAllOther(t *testing.T) {
	t.Parallel()
	body := []byte("data: [DONE]\n\n")
	s := reassembleSSE(body, APIOpenAI)
	if s.ResponseBytes.Total != len(body) {
		t.Errorf("Total = %d, want %d", s.ResponseBytes.Total, len(body))
	}
	if s.ResponseBytes.Text+s.ResponseBytes.Thinking+s.ResponseBytes.ToolCalls != 0 {
		t.Errorf("named sections should be 0 for [DONE]-only stream; got %+v", s.ResponseBytes)
	}
	if s.ResponseBytes.Other != len(body) {
		t.Errorf("Other = %d, want %d (everything is scaffolding)", s.ResponseBytes.Other, len(body))
	}
	if s.Terminated != "normal" {
		t.Errorf("Terminated = %q, want normal", s.Terminated)
	}
}

// TestResponseBytes_OnlyMessageStopStreamAllOther: same shape on
// the Anthropic side. message_stop alone yields all-Other partition,
// Terminated="normal".
func TestResponseBytes_OnlyMessageStopStreamAllOther(t *testing.T) {
	t.Parallel()
	body := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	s := reassembleSSE(body, APIAnthropic)
	if s.ResponseBytes.Total != len(body) {
		t.Errorf("Total = %d, want %d", s.ResponseBytes.Total, len(body))
	}
	if s.ResponseBytes.Text+s.ResponseBytes.Thinking+s.ResponseBytes.ToolCalls != 0 {
		t.Errorf("named sections should be 0; got %+v", s.ResponseBytes)
	}
	if s.ResponseBytes.Other != len(body) {
		t.Errorf("Other = %d, want %d", s.ResponseBytes.Other, len(body))
	}
	if s.Terminated != "normal" {
		t.Errorf("Terminated = %q, want normal", s.Terminated)
	}
}

// TestResponseBytes_TruncatedMidFramePartitionClosesOverParsed:
// stream that ends mid-`data:` line. Partition still closes — the
// trailing partial bytes are unparseable but `iterEvents` skips
// them silently, and Other absorbs whatever the scanner consumed.
func TestResponseBytes_TruncatedMidFramePartitionClosesOverParsed(t *testing.T) {
	t.Parallel()
	body := []byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"complete"}}

event: content_block_delta
data: {"type":"content_block_de`) // mid-frame truncation, no trailing newline-newline
	s := reassembleSSE(body, APIAnthropic)
	rb := s.ResponseBytes
	if rb.Total != len(body) {
		t.Errorf("Total = %d, want %d", rb.Total, len(body))
	}
	if sum := rb.Text + rb.Thinking + rb.ToolCalls + rb.Other; sum != rb.Total {
		t.Errorf("partition broken under truncation: %d != %d", sum, rb.Total)
	}
	// At least the first complete frame was parsed → some text.
	if rb.Text == 0 {
		t.Errorf("expected some text from the complete first frame")
	}
	if s.Terminated != "upstream" {
		t.Errorf("Terminated = %q, want upstream (no marker on truncated stream)", s.Terminated)
	}
}
