package gateway

import (
	"strings"
	"testing"
)

// TestAnalyzeRequest_AnthropicPicksLargestToolResult: three
// tool_results of {10kB, 40kB, 25kB} at message indices {3, 7, 9}
// → top selects the 40kB block at index 7 with the matching tool name.
func TestAnalyzeRequest_AnthropicPicksLargestToolResult(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_a","name":"Read","input":{"file_path":"/a"}},` +
		`{"type":"tool_use","id":"toolu_b","name":"Grep","input":{"pattern":"x","path":"/b"}},` +
		`{"type":"tool_use","id":"toolu_c","name":"Bash","input":{"command":"ls"}}` +
		`]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":` + jsonString(strings.Repeat("a", 10240)) + `}]},` +
		`{"role":"assistant","content":"interim"},` +
		`{"role":"user","content":"more"},` +
		`{"role":"assistant","content":"sure"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":` + jsonString(strings.Repeat("b", 40960)) + `}]},` +
		`{"role":"assistant","content":"sure"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_c","content":` + jsonString(strings.Repeat("c", 25600)) + `}]}` +
		`]}`)

	top, _ := analyzeRequest(body, APIAnthropic, "sess", nil)
	if top == nil {
		t.Fatal("top_tool_result = nil")
	}
	if top.Bytes != 40960 {
		t.Errorf("Bytes = %d, want 40960 (the largest)", top.Bytes)
	}
	if top.TurnIndex != 6 {
		t.Errorf("TurnIndex = %d, want 6 (0-based message index of the 40kB block)", top.TurnIndex)
	}
	if top.ToolUseID != "toolu_b" {
		t.Errorf("ToolUseID = %q, want toolu_b", top.ToolUseID)
	}
	if top.ToolName != "Grep" {
		t.Errorf("ToolName = %q, want Grep (matched via tool_use_id correlation)", top.ToolName)
	}
}

// TestAnalyzeRequest_OrphanToolResultLeavesToolNameEmpty: a
// tool_result whose tool_use_id matches no earlier tool_use yields
// ToolName="" and a non-empty ToolUseID. The struct is still returned
// with the size + index — just no tool name to attribute it to.
func TestAnalyzeRequest_OrphanToolResultLeavesToolNameEmpty(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"abc"}]}` +
		`]}`)
	top, _ := analyzeRequest(body, APIAnthropic, "sess", nil)
	if top == nil {
		t.Fatal("top_tool_result = nil; orphan tool_result still produces a record")
	}
	if top.ToolUseID != "orphan" {
		t.Errorf("ToolUseID = %q, want %q", top.ToolUseID, "orphan")
	}
	if top.ToolName != "" {
		t.Errorf("ToolName = %q, want empty (no matching tool_use)", top.ToolName)
	}
}

// TestAnalyzeRequest_NoToolResultsYieldsNil: a request with no
// tool_result blocks at all returns nil for top_tool_result. The
// audit row writer omits the field on nil per the §4.6 presence
// rule.
func TestAnalyzeRequest_NoToolResultsYieldsNil(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"hello"}` +
		`]}`)
	top, _ := analyzeRequest(body, APIAnthropic, "sess", nil)
	if top != nil {
		t.Errorf("top_tool_result = %+v, want nil", top)
	}
}

// TestAnalyzeRequest_AnthropicRepeatReadsAccumulatesAcrossTurns:
// three turns of Read calls with overlapping file paths land
// repeat_reads as expected from the §7.6 fixtures.
func TestAnalyzeRequest_AnthropicRepeatReadsAccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()
	idx := newReadIndex()

	turn := func(filepath string) []byte {
		return []byte(`{"model":"x","messages":[{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"u1","name":"Read","input":{"file_path":` + jsonString(filepath) + `}}` +
			`]}]}`)
	}

	_, r1 := analyzeRequest(turn("/foo"), APIAnthropic, "sess", idx)
	_, r2 := analyzeRequest(turn("/foo"), APIAnthropic, "sess", idx)

	if r1 != nil {
		t.Errorf("turn 1 repeat_reads = %+v, want nil (no prior history, max=1 is trivial)", r1)
	}
	if r2 == nil {
		t.Fatal("turn 2 repeat_reads = nil, want populated")
	}
	if r2.Count != 1 || r2.MaxSamePath != 2 {
		t.Errorf("turn 2: count=%d max=%d, want 1,2", r2.Count, r2.MaxSamePath)
	}
}

// TestAnalyzeRequest_NilReadIndexProducesNilRepeatReads: passing
// idx=nil is a supported "skip repeat tracking" mode. top is still
// computed.
func TestAnalyzeRequest_NilReadIndexProducesNilRepeatReads(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"u","name":"Read","input":{"file_path":"/foo"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":"x"}]}` +
		`]}`)
	top, repeat := analyzeRequest(body, APIAnthropic, "sess", nil)
	if repeat != nil {
		t.Errorf("repeat_reads = %+v, want nil with idx=nil", repeat)
	}
	if top == nil {
		t.Error("top_tool_result = nil; should still compute even without idx")
	}
}

// TestAnalyzeRequest_OpenAIToolMessages: OpenAI carries tool results
// in messages with role="tool". analyzeOpenAI picks the largest of
// those and links to the assistant's tool_calls[].id.
func TestAnalyzeRequest_OpenAIToolMessages(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a\"}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"Grep","arguments":"{\"pattern\":\"x\",\"path\":\"/b\"}"}}` +
		`]},` +
		`{"role":"tool","tool_call_id":"call_1","content":` + jsonString(strings.Repeat("a", 100)) + `},` +
		`{"role":"tool","tool_call_id":"call_2","content":` + jsonString(strings.Repeat("b", 500)) + `}` +
		`]}`)
	top, _ := analyzeRequest(body, APIOpenAI, "sess", nil)
	if top == nil {
		t.Fatal("top_tool_result = nil for OpenAI tool messages")
	}
	if top.Bytes != 500 {
		t.Errorf("Bytes = %d, want 500 (the larger tool message)", top.Bytes)
	}
	if top.ToolUseID != "call_2" {
		t.Errorf("ToolUseID = %q, want call_2", top.ToolUseID)
	}
	if top.ToolName != "Grep" {
		t.Errorf("ToolName = %q, want Grep", top.ToolName)
	}
}

// TestAnalyzeRequest_OpenAIToolArgsUnwrapping: OpenAI emits tool
// arguments as a JSON-encoded *string* rather than an object. The
// canonical key derivation must unwrap that string before applying
// CanonicalToolArg, otherwise repeat_reads keys never match across
// translation directions.
func TestAnalyzeRequest_OpenAIToolArgsUnwrapping(t *testing.T) {
	t.Parallel()
	idx := newReadIndex()
	turn := func() []byte {
		return []byte(`{"model":"gpt-4o","messages":[` +
			`{"role":"assistant","content":null,"tool_calls":[` +
			`{"id":"call_x","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/foo\"}"}}` +
			`]}]}`)
	}
	_, r1 := analyzeRequest(turn(), APIOpenAI, "sess", idx)
	_, r2 := analyzeRequest(turn(), APIOpenAI, "sess", idx)
	if r1 != nil {
		t.Errorf("turn 1 repeat_reads = %+v, want nil", r1)
	}
	if r2 == nil || r2.MaxSamePath != 2 {
		t.Errorf("turn 2 repeat_reads = %+v, want max_same_path=2 (proves arguments unwrapped before canonicalization)", r2)
	}
}

// TestAnalyzeRequest_MalformedReturnsBothNil: garbage input doesn't
// crash and produces nil/nil so the audit row's presence rules
// trivially omit the fields.
func TestAnalyzeRequest_MalformedReturnsBothNil(t *testing.T) {
	t.Parallel()
	top, repeat := analyzeRequest([]byte(`not-json`), APIAnthropic, "sess", newReadIndex())
	if top != nil || repeat != nil {
		t.Errorf("malformed: top=%+v repeat=%+v, want nil,nil", top, repeat)
	}
}

// TestAnalyzeRequest_EmptyBodyReturnsBothNil: same for an empty
// body.
func TestAnalyzeRequest_EmptyBodyReturnsBothNil(t *testing.T) {
	t.Parallel()
	top, repeat := analyzeRequest(nil, APIAnthropic, "sess", newReadIndex())
	if top != nil || repeat != nil {
		t.Errorf("empty: top=%+v repeat=%+v, want nil,nil", top, repeat)
	}
}

// TestAnalyzeRequest_UnknownAPIReturnsBothNil: unknown clientAPI is
// the audit equivalent of the SectionBytes "unknown api" case —
// return nothing rather than misclassify.
func TestAnalyzeRequest_UnknownAPIReturnsBothNil(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[]}`)
	top, repeat := analyzeRequest(body, "passthrough-magic", "sess", newReadIndex())
	if top != nil || repeat != nil {
		t.Errorf("unknown api: top=%+v repeat=%+v, want nil,nil", top, repeat)
	}
}
