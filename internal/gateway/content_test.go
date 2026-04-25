package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestSectionBytes_PartitionClosure is the §4.1 I2 invariant lifted
// straight into a test. For every fixture body, sum of all nine
// section fields equals len(body) — exactly, no tolerance.
//
// This single assertion catches every category of bug at once:
// overlap, double-count, missed bytes. If it fails on a fixture that
// per-section value tests pass on, the bug is in the partition design,
// not in any one section's implementation.
func TestSectionBytes_PartitionClosure(t *testing.T) {
	t.Parallel()
	for _, fx := range buildFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			sb := SectionBytes(fx.body, fx.api)
			if sb.Total() != len(fx.body) {
				t.Fatalf("partition broken: sum=%d len(body)=%d delta=%d\n"+
					"  System=%d Tools=%d UserText=%d Preamble=%d\n"+
					"  ToolResults=%d Thinking=%d ImagesWire=%d\n"+
					"  UserHistory=%d Other=%d",
					sb.Total(), len(fx.body), len(fx.body)-sb.Total(),
					sb.System, sb.Tools, sb.UserText, sb.Preamble,
					sb.ToolResults, sb.Thinking, sb.ImagesWire,
					sb.UserHistory, sb.Other)
			}
			// Other must be non-negative — a negative value would
			// signal that named sections double-counted somewhere.
			if sb.Other < 0 {
				t.Fatalf("Other=%d (negative): a named section must have double-counted", sb.Other)
			}
		})
	}
}

// TestSectionBytes_Anthropic1MiBImage pins the §7.3 image-counting
// contract: a 1 MiB source image becomes a 1_398_104-byte base64
// string on the wire (= ceil(1048576/3)*4), and ImagesWire counts
// exactly that — not the data: prefix, not the JSON quotes, not the
// surrounding source object.
func TestSectionBytes_Anthropic1MiBImage(t *testing.T) {
	t.Parallel()
	const sourceBytes = 1024 * 1024
	const wantBase64 = 1_398_104

	imgRaw := make([]byte, sourceBytes)
	if _, err := rand.Read(imgRaw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(imgRaw)
	if len(b64) != wantBase64 {
		t.Fatalf("base64 length = %d, want %d (test fixture wrong)", len(b64), wantBase64)
	}

	body := buildAnthropicWithImage(t, b64)
	sb := SectionBytes(body, APIAnthropic)

	if sb.ImagesWire != wantBase64 {
		t.Errorf("ImagesWire = %d, want %d (decoded base64 length only)", sb.ImagesWire, wantBase64)
	}
	if sb.Total() != len(body) {
		t.Errorf("partition broken on image fixture: sum=%d len(body)=%d", sb.Total(), len(body))
	}
}

// TestSectionBytes_OpenAIImageWireExcludesDataURIPrefix walks the
// trap reviewer flagged: the data:image/png;base64, prefix is
// scaffolding, not image content. Only the bytes after the comma
// count toward ImagesWire.
func TestSectionBytes_OpenAIImageWireExcludesDataURIPrefix(t *testing.T) {
	t.Parallel()
	const payloadLen = 4096
	imgRaw := make([]byte, payloadLen)
	_, _ = rand.Read(imgRaw)
	b64 := base64.StdEncoding.EncodeToString(imgRaw)

	dataURI := "data:image/png;base64," + b64

	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"look at this image"},` +
		`{"type":"image_url","image_url":{"url":` + jsonString(dataURI) + `}}` +
		`]}]}`)

	sb := SectionBytes(body, APIOpenAI)
	if sb.ImagesWire != len(b64) {
		t.Errorf("ImagesWire = %d, want %d (base64 only, no data: prefix)", sb.ImagesWire, len(b64))
	}
	if sb.Total() != len(body) {
		t.Errorf("partition broken: sum=%d len(body)=%d", sb.Total(), len(body))
	}
}

// TestSectionBytes_OpenAIImageURLPlainURLNotCounted: a plain http(s)
// image URL contributes 0 to ImagesWire — the URL string is JSON
// scaffolding under §4.1; only embedded base64 is "image bytes".
func TestSectionBytes_OpenAIImageURLPlainURLNotCounted(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}` +
		`]}]}`)
	sb := SectionBytes(body, APIOpenAI)
	if sb.ImagesWire != 0 {
		t.Errorf("ImagesWire = %d for plain URL, want 0", sb.ImagesWire)
	}
	if sb.Total() != len(body) {
		t.Errorf("partition broken: sum=%d len(body)=%d", sb.Total(), len(body))
	}
}

// TestSectionBytes_AnthropicHandcraftedValues exercises one fixture
// with hand-known per-section sizes so a bug in any one branch
// surfaces with a specific failure rather than a partition closure
// fault hiding the actual offset.
func TestSectionBytes_AnthropicHandcraftedValues(t *testing.T) {
	t.Parallel()
	systemText := "You are a helpful assistant."
	userText := "What does main.go do?"
	thinkingText := "Reading the file..."
	toolResultText := strings.Repeat("x", 100)

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
		`"system":` + jsonString(systemText) + `,` +
		`"messages":[` +
		`{"role":"user","content":` + jsonString(userText) + `},` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":` + jsonString(thinkingText) + `},` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/foo"}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_1","content":` + jsonString(toolResultText) + `}` +
		`]}` +
		`]}`)

	sb := SectionBytes(body, APIAnthropic)

	if sb.System != len(systemText) {
		t.Errorf("System = %d, want %d (decoded systemText length)", sb.System, len(systemText))
	}
	if sb.UserText != 0 {
		t.Errorf("UserText = %d, want 0 (latest user message has only tool_result, no text)", sb.UserText)
	}
	if sb.UserHistory != len(userText) {
		t.Errorf("UserHistory = %d, want %d (earlier user msg's text)", sb.UserHistory, len(userText))
	}
	if sb.Thinking != len(thinkingText) {
		t.Errorf("Thinking = %d, want %d", sb.Thinking, len(thinkingText))
	}
	if sb.ToolResults != len(toolResultText) {
		t.Errorf("ToolResults = %d, want %d", sb.ToolResults, len(toolResultText))
	}
	if sb.Total() != len(body) {
		t.Errorf("partition: sum=%d len(body)=%d", sb.Total(), len(body))
	}
}

// TestSectionBytes_AnthropicSystemBlockArray asserts both system shapes
// (string vs block array of text blocks) yield the same decoded count
// for the same logical content. The block-array form has more JSON
// scaffolding; that scaffolding lands in Other, not System.
func TestSectionBytes_AnthropicSystemBlockArray(t *testing.T) {
	t.Parallel()
	const sysText = "You are a helpful assistant."
	stringBody := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"system":` + jsonString(sysText) + `}`)
	arrayBody := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":` + jsonString(sysText) + `}]}`)

	sbStr := SectionBytes(stringBody, APIAnthropic)
	sbArr := SectionBytes(arrayBody, APIAnthropic)

	if sbStr.System != len(sysText) {
		t.Errorf("string-form System = %d, want %d", sbStr.System, len(sysText))
	}
	if sbArr.System != len(sysText) {
		t.Errorf("array-form System = %d, want %d (block-array scaffolding belongs to Other, not System)", sbArr.System, len(sysText))
	}
	if sbStr.Total() != len(stringBody) {
		t.Errorf("string partition: sum=%d len=%d", sbStr.Total(), len(stringBody))
	}
	if sbArr.Total() != len(arrayBody) {
		t.Errorf("array partition: sum=%d len=%d", sbArr.Total(), len(arrayBody))
	}
}

// TestSectionBytes_MalformedYieldsAllOther: an unparseable body must
// still satisfy I2. Other absorbs everything when no fields can be
// classified.
func TestSectionBytes_MalformedYieldsAllOther(t *testing.T) {
	t.Parallel()
	body := []byte(`not valid json at all`)
	sb := SectionBytes(body, APIAnthropic)
	if sb.Other != len(body) {
		t.Errorf("Other = %d, want %d (malformed → all to Other)", sb.Other, len(body))
	}
	if sb.Total() != len(body) {
		t.Errorf("partition closure violated for malformed: %d vs %d", sb.Total(), len(body))
	}
}

// TestSectionBytes_EmptyBody returns the zero value, partition closure
// holds trivially.
func TestSectionBytes_EmptyBody(t *testing.T) {
	t.Parallel()
	sb := SectionBytes(nil, APIAnthropic)
	if sb.Total() != 0 {
		t.Errorf("Total = %d for empty body, want 0", sb.Total())
	}
}

// TestSectionBytes_UnknownAPIYieldsAllOther: callers passing a bogus
// API name (or a passthrough mode that doesn't claim either schema)
// see all bytes lumped into Other instead of misclassified.
func TestSectionBytes_UnknownAPIYieldsAllOther(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	sb := SectionBytes(body, "passthrough-magic")
	if sb.Other != len(body) {
		t.Errorf("Other = %d, want %d (unknown api → all Other)", sb.Other, len(body))
	}
}

// --- fixture builders ---

type contentFixture struct {
	name string
	body []byte
	api  string
}

func buildFixtures(t *testing.T) []contentFixture {
	t.Helper()
	return []contentFixture{
		{
			name: "anthropic_string_content",
			api:  APIAnthropic,
			body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
				`"system":"You are a helpful assistant.",` +
				`"messages":[` +
				`{"role":"user","content":"What is 2+2?"},` +
				`{"role":"assistant","content":"4"},` +
				`{"role":"user","content":"And 3+3?"}` +
				`]}`),
		},
		{
			name: "anthropic_block_array_with_tool_pair",
			api:  APIAnthropic,
			body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
				`"system":"sys",` +
				`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
				`"messages":[` +
				`{"role":"user","content":"<system-reminder>injected</system-reminder>read main.go please"},` +
				`{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"need to read"},` +
				`{"type":"text","text":"sure"},` +
				`{"type":"tool_use","id":"toolu_xy","name":"Read","input":{"file_path":"/main.go"}}` +
				`]},` +
				`{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"toolu_xy","content":"package main"}` +
				`]}` +
				`]}`),
		},
		{
			name: "anthropic_with_1mib_image",
			api:  APIAnthropic,
			body: buildAnthropicWithImage(t, base64.StdEncoding.EncodeToString(deterministicBytes(1024*1024))),
		},
		{
			name: "openai_string_content",
			api:  APIOpenAI,
			body: []byte(`{"model":"gpt-4o","messages":[` +
				`{"role":"system","content":"You are a helpful assistant."},` +
				`{"role":"user","content":"What is 2+2?"},` +
				`{"role":"assistant","content":"4"},` +
				`{"role":"user","content":"And 3+3?"}` +
				`]}`),
		},
		{
			name: "openai_multimodal_image_url_and_text",
			api:  APIOpenAI,
			body: buildOpenAIMultimodal(t),
		},
	}
}

// buildAnthropicWithImage constructs an Anthropic body with a single
// base64 image block. The base64 is supplied by the caller so the
// test can measure ImagesWire against a known length.
func buildAnthropicWithImage(t *testing.T, b64 string) []byte {
	t.Helper()
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
		`"messages":[` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"describe this image"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":` + jsonString(b64) + `}}` +
		`]}` +
		`]}`)
	return body
}

func buildOpenAIMultimodal(t *testing.T) []byte {
	t.Helper()
	const payloadLen = 1024
	b64 := base64.StdEncoding.EncodeToString(deterministicBytes(payloadLen))
	dataURI := "data:image/png;base64," + b64
	return []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"caption this"},` +
		`{"type":"image_url","image_url":{"url":` + jsonString(dataURI) + `}}` +
		`]}]}`)
}

// jsonString emits a JSON-quoted string for inline body construction.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// deterministicBytes returns n bytes derived from a fixed seed so
// fixture sizes are reproducible across runs. crypto/rand would also
// work but produces different content per run, complicating golden
// comparisons later.
func deterministicBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i % 256)
	}
	return out
}
