package gateway

import (
	"encoding/json"
	"regexp"
	"strings"
)

// SectionByteCounts is the §4.1 partition of a request body's bytes
// into nine disjoint sections plus "other" for JSON scaffolding. Two
// invariants govern every count:
//
//   - I1 (disjoint): each byte of the body belongs to exactly one
//     field. Sections never overlap.
//   - I2 (sum-to-total): Total() == len(body) exactly. No tolerance.
//     The Other field absorbs all bytes not assigned to a named
//     section, so the partition always closes.
//
// Counting convention. String-valued payload bytes (system text,
// content text, thinking, tool_result text) are counted by their
// DECODED string length — quotes and JSON escaping go to Other.
// Structure-valued payloads (tools array, image-block wrappers) are
// counted by their wire-JSON length. Images are special-cased: only
// the raw base64 payload bytes are ImagesWire; data: URI prefix,
// media-type field, surrounding object scaffolding all go to Other.
//
// The breakdown is for observability, not admission control. Use
// estimateTokens(body) for sizing decisions.
type SectionByteCounts struct {
	System      int
	Tools       int
	UserText    int
	Preamble    int
	ToolResults int
	Thinking    int
	ImagesWire  int
	UserHistory int
	Other       int
}

// Total sums all nine fields. Equals len(body) by I2 for any
// well-formed input that SectionBytes parsed successfully.
func (s SectionByteCounts) Total() int {
	return s.System + s.Tools + s.UserText + s.Preamble +
		s.ToolResults + s.Thinking + s.ImagesWire +
		s.UserHistory + s.Other
}

// SectionBytes returns the §4.1 partition for an Anthropic or OpenAI
// request body. Malformed or unparseable JSON yields a single-field
// result with Other == len(body), preserving I2.
func SectionBytes(body []byte, clientAPI string) SectionByteCounts {
	if len(body) == 0 {
		return SectionByteCounts{}
	}
	var sb SectionByteCounts
	switch clientAPI {
	case APIAnthropic:
		sb = sectionBytesAnthropic(body)
	case APIOpenAI:
		sb = sectionBytesOpenAI(body)
	default:
		return SectionByteCounts{Other: len(body)}
	}
	// I2 closes by construction: Other absorbs the remainder.
	classified := sb.System + sb.Tools + sb.UserText + sb.Preamble +
		sb.ToolResults + sb.Thinking + sb.ImagesWire + sb.UserHistory
	sb.Other = len(body) - classified
	return sb
}

// --- Anthropic ---

type anthropicShell struct {
	System   json.RawMessage   `json:"system"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

type anthropicMsgEnv struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func sectionBytesAnthropic(body []byte) SectionByteCounts {
	var shell anthropicShell
	if err := json.Unmarshal(body, &shell); err != nil {
		return SectionByteCounts{Other: len(body)}
	}
	var sb SectionByteCounts

	// system: string or block-array. Both forms count by decoded text length.
	if len(shell.System) > 0 {
		sb.System = anthropicSystemBytes(shell.System)
	}

	// tools: count the raw wire-JSON length of the array.
	if len(shell.Tools) > 0 {
		sb.Tools = len(shell.Tools)
	}

	// messages: walk; classify per-message based on role + content shape.
	lastUserIdx := lastUserMessageIndex(shell.Messages)
	for i, raw := range shell.Messages {
		var env anthropicMsgEnv
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		isLatestUser := env.Role == "user" && i == lastUserIdx
		anthropicAccumulateMessage(&sb, env, isLatestUser)
	}

	return sb
}

func anthropicSystemBytes(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	if raw[0] == '"' {
		return decodedStringLen(raw)
	}
	// Block array: sum text content of each text block.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		if b.Type == "text" {
			n += len(b.Text)
		}
	}
	return n
}

func lastUserMessageIndex(msgs []json.RawMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		var env anthropicMsgEnv
		if err := json.Unmarshal(msgs[i], &env); err == nil && env.Role == "user" {
			return i
		}
	}
	return -1
}

func anthropicAccumulateMessage(sb *SectionByteCounts, env anthropicMsgEnv, isLatestUser bool) {
	if env.Role == "user" {
		if isLatestUser {
			anthropicWalkUserContent(sb, env.Content, &sb.UserText, &sb.Preamble, true)
		} else {
			anthropicWalkUserContent(sb, env.Content, &sb.UserHistory, &sb.UserHistory, false)
		}
		return
	}
	// assistant
	anthropicWalkAssistantContent(sb, env.Content)
}

// anthropicWalkUserContent walks a user message's content and adds bytes
// to the appropriate section. textTarget points at UserText (latest)
// or UserHistory (earlier turns). preambleTarget likewise.
//
// For string content, the decoded text minus preamble goes to textTarget,
// preamble bytes to preambleTarget. For block-array content, text blocks
// are split the same way; tool_result blocks contribute to ToolResults;
// image blocks contribute to ImagesWire (raw base64 payload only).
func anthropicWalkUserContent(sb *SectionByteCounts, content json.RawMessage, textTarget, preambleTarget *int, splitPreamble bool) {
	if len(content) == 0 {
		return
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return
		}
		if splitPreamble {
			tb, pb := splitTextAndPreamble(s)
			*textTarget += tb
			*preambleTarget += pb
		} else {
			*textTarget += len(s)
		}
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Source    json.RawMessage `json:"source"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if splitPreamble {
				tb, pb := splitTextAndPreamble(b.Text)
				*textTarget += tb
				*preambleTarget += pb
			} else {
				*textTarget += len(b.Text)
			}
		case "tool_result":
			sb.ToolResults += anthropicToolResultBytes(b.Content)
		case "image":
			sb.ImagesWire += anthropicImageBytes(b.Source)
		}
	}
}

func anthropicWalkAssistantContent(sb *SectionByteCounts, content json.RawMessage) {
	if len(content) == 0 {
		return
	}
	if content[0] == '"' {
		// Plain assistant prose — lumped into Other per §4.1 (no
		// AssistantHistory bucket in v1). Adding nothing here is
		// correct: Other absorbs whatever isn't claimed.
		return
	}
	var blocks []struct {
		Type     string `json:"type"`
		Thinking string `json:"thinking"`
		// text and tool_use blocks both go to Other implicitly.
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "thinking" {
			sb.Thinking += len(b.Thinking)
		}
	}
}

// anthropicToolResultBytes returns the decoded content size of a
// tool_result block. Content can be a string or a block array.
func anthropicToolResultBytes(content json.RawMessage) int {
	if len(content) == 0 {
		return 0
	}
	if content[0] == '"' {
		return decodedStringLen(content)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		if b.Type == "text" {
			n += len(b.Text)
		}
	}
	return n
}

// anthropicImageBytes returns the decoded length of the base64 data in
// an image block's source. For URL-source images, returns 0 (the URL
// itself is scaffolding under the spec; only base64 payload counts).
//
// The 1 MiB image test case from SPEC §7.3 hits this with a base64
// string of length 1_398_104 = ceil(1048576/3)*4 — matched exactly
// because we decode the JSON-quoted string and report its rune length.
func anthropicImageBytes(source json.RawMessage) int {
	if len(source) == 0 {
		return 0
	}
	var s struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(source, &s); err != nil {
		return 0
	}
	if s.Type == "base64" {
		return len(s.Data)
	}
	return 0
}

// --- OpenAI ---

type openaiShell struct {
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

type openaiMsgEnv struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func sectionBytesOpenAI(body []byte) SectionByteCounts {
	var shell openaiShell
	if err := json.Unmarshal(body, &shell); err != nil {
		return SectionByteCounts{Other: len(body)}
	}
	var sb SectionByteCounts

	if len(shell.Tools) > 0 {
		sb.Tools = len(shell.Tools)
	}

	// system in OpenAI is the first message with role=="system".
	// All system-role messages contribute to System (rare to have
	// more than one).
	lastUserIdx := lastOpenAIUserMessageIndex(shell.Messages)
	for i, raw := range shell.Messages {
		var env openaiMsgEnv
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Role {
		case "system":
			sb.System += openaiTextLen(env.Content)
		case "user":
			isLatestUser := i == lastUserIdx
			openaiWalkUserContent(&sb, env.Content, isLatestUser)
		case "tool":
			sb.ToolResults += openaiTextLen(env.Content)
		default:
			// assistant, function: assistant prose lumps into Other
		}
	}
	return sb
}

func lastOpenAIUserMessageIndex(msgs []json.RawMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		var env openaiMsgEnv
		if err := json.Unmarshal(msgs[i], &env); err == nil && env.Role == "user" {
			return i
		}
	}
	return -1
}

// openaiTextLen returns the decoded string length of an OpenAI content
// field that is a single string. Returns 0 when content is a multimodal
// array; callers that need to walk arrays use openaiWalkUserContent.
func openaiTextLen(content json.RawMessage) int {
	if len(content) == 0 || content[0] != '"' {
		return 0
	}
	return decodedStringLen(content)
}

// openaiWalkUserContent handles both string content and the multimodal
// array form ([{type:"text",text:...}, {type:"image_url",image_url:{url:...}}]).
func openaiWalkUserContent(sb *SectionByteCounts, content json.RawMessage, isLatestUser bool) {
	if len(content) == 0 {
		return
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return
		}
		if isLatestUser {
			tb, pb := splitTextAndPreamble(s)
			sb.UserText += tb
			sb.Preamble += pb
		} else {
			sb.UserHistory += len(s)
		}
		return
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			if isLatestUser {
				tb, pb := splitTextAndPreamble(p.Text)
				sb.UserText += tb
				sb.Preamble += pb
			} else {
				sb.UserHistory += len(p.Text)
			}
		case "image_url":
			sb.ImagesWire += openaiImageWireBytes(p.ImageURL.URL)
		}
	}
}

// openaiImageWireBytes extracts the base64 payload bytes from a data:
// URI. For non-data URLs (plain http(s) links), returns 0 — the URL
// itself is JSON scaffolding; only embedded base64 counts as image
// payload per §4.1.
func openaiImageWireBytes(url string) int {
	if !strings.HasPrefix(url, "data:") {
		return 0
	}
	idx := strings.Index(url, ",")
	if idx < 0 {
		return 0
	}
	return len(url[idx+1:])
}

// --- shared helpers ---

// decodedStringLen returns len(decoded) for a JSON-quoted string raw
// message. Returns 0 on parse failure.
func decodedStringLen(raw json.RawMessage) int {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	return len(s)
}

// preambleTagOpen matches the opening of a pseudo-XML preamble block
// like <system-reminder> or <command-name>. Mirrors the existing
// audit_stats.go customTagOpen heuristic so this implementation stays
// in step with the admin UI's view of "preamble" content. (Cross-
// package consolidation lands in 7b.)
var preambleTagOpen = regexp.MustCompile(`(?i)<([a-z][a-z0-9-]{2,40})\b[^>]*>`)

// splitTextAndPreamble returns (textBytes, preambleBytes) for a piece
// of user-message text. Preamble = pseudo-XML blocks at the leading or
// trailing edge; user_text = whatever the user actually typed in
// between. Mirrors scanPreambleTags from audit_stats.go.
//
// Sum invariant: textBytes + preambleBytes == len(s) exactly.
// (Whitespace between blocks is counted as text.)
func splitTextAndPreamble(s string) (textBytes, preambleBytes int) {
	if s == "" {
		return 0, 0
	}
	type span struct{ start, end int }
	opens := preambleTagOpen.FindAllStringSubmatchIndex(s, -1)
	if len(opens) == 0 {
		return len(s), 0
	}
	var spans []span
	for _, om := range opens {
		name := strings.ToLower(s[om[2]:om[3]])
		closeTag := "</" + name + ">"
		tail := s[om[1]:]
		idx := strings.Index(strings.ToLower(tail), closeTag)
		if idx < 0 {
			continue
		}
		spans = append(spans, span{om[0], om[1] + idx + len(closeTag)})
	}
	if len(spans) == 0 {
		return len(s), 0
	}
	// First-typed / last-typed positions: characters outside spans and
	// outside whitespace. Mirrors scanPreambleTags so the preamble vs
	// text boundary lands the same way the admin UI sees it today.
	inSpan := func(i int) bool {
		for _, sp := range spans {
			if i >= sp.start && i < sp.end {
				return true
			}
		}
		return false
	}
	firstTyped, lastTyped := -1, -1
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if inSpan(i) {
			continue
		}
		if firstTyped == -1 {
			firstTyped = i
		}
		lastTyped = i
	}
	// Preamble counts only the spans at the leading/trailing edges (no
	// typed content before/after them).
	for _, sp := range spans {
		isLeading := firstTyped == -1 || sp.end <= firstTyped
		isTrailing := lastTyped == -1 || sp.start > lastTyped
		if isLeading || isTrailing {
			preambleBytes += sp.end - sp.start
		}
	}
	textBytes = len(s) - preambleBytes
	return textBytes, preambleBytes
}
