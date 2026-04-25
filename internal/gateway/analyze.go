package gateway

import (
	"encoding/json"
)

// TopToolResultInfo describes the single largest tool_result block in
// a request body, plus enough metadata to find it again from raw
// JSONL via jq.
//
// Persisted in the audit response row as `top_tool_result`. Omitted
// when no tool_result blocks are present.
type TopToolResultInfo struct {
	// Bytes is the decoded content size of the tool_result block,
	// using the same convention as SectionByteCounts.ToolResults
	// (string content → decoded len; block-array content → sum of
	// text-block lengths). Aligns with the partition unit.
	Bytes int `json:"bytes"`
	// TurnIndex is the 0-based index of the message (in
	// req.Messages) that contained the block.
	TurnIndex int `json:"turn_index"`
	// ToolName is the assistant tool_use's `name` field when the
	// tool_result's tool_use_id matches a tool_use earlier in the
	// conversation. Empty when no match (orphan tool_result).
	ToolName string `json:"tool_name,omitempty"`
	// ToolUseID is the tool_result's tool_use_id field. Anthropic
	// permits tool_result blocks without a prior tool_use, in which
	// case this is empty (omitempty per §4.5 nullability rule).
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// RepeatReadsInfo summarizes re-read activity within a session.
// Persisted in the audit response row as `repeat_reads`.
//
// Omitted from the row when both fields are zero/one (per §4.6
// field-presence rule: count==0 AND max_same_path<=1).
type RepeatReadsInfo struct {
	// Count is the number of distinct canonical tool args in THIS
	// request that were already seen in an earlier request of the
	// same session. Intra-request duplicates do not inflate Count.
	Count int `json:"count"`
	// MaxSamePath is the max total occurrence of any single
	// canonical key across all turns of the session, including
	// intra-turn duplicates and including this request's
	// occurrences.
	MaxSamePath int `json:"max_same_path"`
}

// analyzeRequest computes top_tool_result and repeat_reads for an
// Anthropic or OpenAI request body. It is the integration seam that
// bundles SectionBytes-adjacent walking with per-session state.
//
// Returns (nil, nil) for any branch where the produced struct would
// be omitted from the audit row anyway:
//   - top_tool_result: nil when the request contains no tool_result blocks.
//   - repeat_reads: nil when count==0 and max_same_path<=1 (the
//     §4.6 omission rule).
//
// idx may be nil (then repeat_reads is always nil — useful in the
// unit tests for SectionBytes-adjacent helpers).
func analyzeRequest(body []byte, clientAPI string, sessionID string, idx *readIndex) (*TopToolResultInfo, *RepeatReadsInfo) {
	if len(body) == 0 {
		return nil, nil
	}
	switch clientAPI {
	case APIAnthropic:
		return analyzeAnthropic(body, sessionID, idx)
	case APIOpenAI:
		return analyzeOpenAI(body, sessionID, idx)
	default:
		return nil, nil
	}
}

// --- Anthropic ---

func analyzeAnthropic(body []byte, sessionID string, idx *readIndex) (*TopToolResultInfo, *RepeatReadsInfo) {
	var shell anthropicShell
	if err := json.Unmarshal(body, &shell); err != nil {
		return nil, nil
	}

	// Two-pass: first build tool_use_id → tool_name from assistant
	// turns, plus collect canonical tool args. Then walk again to
	// find the largest tool_result and link its tool_use_id.
	useNames := make(map[string]string)
	var keys []string
	var top *TopToolResultInfo

	for i, raw := range shell.Messages {
		var env anthropicMsgEnv
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Role {
		case "assistant":
			collectAnthropicAssistantBlocks(env.Content, useNames, &keys)
		case "user":
			top = updateTopAnthropicToolResult(env.Content, i, top)
		}
	}

	if top != nil && top.ToolUseID != "" {
		if name, ok := useNames[top.ToolUseID]; ok {
			top.ToolName = name
		}
	}

	var repeat *RepeatReadsInfo
	if idx != nil && len(keys) > 0 {
		count, maxSame := idx.observe(sessionID, keys)
		if count > 0 || maxSame > 1 {
			repeat = &RepeatReadsInfo{Count: count, MaxSamePath: maxSame}
		}
	}
	return top, repeat
}

func collectAnthropicAssistantBlocks(content json.RawMessage, useNames map[string]string, keys *[]string) {
	if len(content) == 0 || content[0] != '[' {
		return
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID != "" {
			useNames[b.ID] = b.Name
		}
		if k := CanonicalToolArg(b.Name, b.Input); k != "" {
			*keys = append(*keys, k)
		}
	}
}

// updateTopAnthropicToolResult walks a user message's content and
// updates `top` if any contained tool_result block is larger than the
// current best. msgIdx is recorded in TurnIndex.
func updateTopAnthropicToolResult(content json.RawMessage, msgIdx int, top *TopToolResultInfo) *TopToolResultInfo {
	if len(content) == 0 || content[0] != '[' {
		return top
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return top
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		size := anthropicToolResultBytes(b.Content)
		if top == nil || size > top.Bytes {
			top = &TopToolResultInfo{
				Bytes:     size,
				TurnIndex: msgIdx,
				ToolUseID: b.ToolUseID,
			}
		}
	}
	return top
}

// --- OpenAI ---

func analyzeOpenAI(body []byte, sessionID string, idx *readIndex) (*TopToolResultInfo, *RepeatReadsInfo) {
	var shell openaiShell
	if err := json.Unmarshal(body, &shell); err != nil {
		return nil, nil
	}

	useNames := make(map[string]string)
	var keys []string
	var top *TopToolResultInfo

	for i, raw := range shell.Messages {
		var env struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Role {
		case "assistant":
			for _, tc := range env.ToolCalls {
				if tc.ID != "" {
					useNames[tc.ID] = tc.Function.Name
				}
				// OpenAI tool arguments are a JSON-string-encoded
				// payload (the model emits a string containing JSON).
				// CanonicalToolArg expects RawMessage of the parsed
				// value, so unwrap once.
				inner := unwrapOpenAIToolArgs(tc.Function.Arguments)
				if k := CanonicalToolArg(tc.Function.Name, inner); k != "" {
					keys = append(keys, k)
				}
			}
		case "tool":
			// tool messages carry tool_result content in the
			// `content` field; the message references back via
			// `tool_call_id`.
			size := openaiTextLen(env.Content)
			if size > 0 {
				if top == nil || size > top.Bytes {
					top = &TopToolResultInfo{
						Bytes:     size,
						TurnIndex: i,
						ToolUseID: env.ToolCallID,
					}
				}
			}
		}
	}

	if top != nil && top.ToolUseID != "" {
		if name, ok := useNames[top.ToolUseID]; ok {
			top.ToolName = name
		}
	}

	var repeat *RepeatReadsInfo
	if idx != nil && len(keys) > 0 {
		count, maxSame := idx.observe(sessionID, keys)
		if count > 0 || maxSame > 1 {
			repeat = &RepeatReadsInfo{Count: count, MaxSamePath: maxSame}
		}
	}
	return top, repeat
}

// unwrapOpenAIToolArgs handles the OpenAI quirk that tool call
// arguments are emitted as a JSON-encoded *string* containing JSON,
// not as a JSON object directly. e.g. `"arguments": "{\"x\":1}"`.
// CanonicalToolArg's fallback (sha256 of raw bytes) would otherwise
// hash the doubly-encoded string and never collide with the
// equivalent Anthropic tool_use input. Unwrap once so canonical keys
// are stable across translation directions.
//
// Format assumption: OpenAI Chat Completions API as of 2024-2026.
// The wire shape for tool_calls[].function.arguments is a JSON
// string. Verified against the upstream spec; stable on api.openai.com
// across the gpt-4 / gpt-4o / o-series families. OpenAI-compatible
// providers (Azure OpenAI, Huawei panshi, OneAPI, vLLM, LiteLLM, Ollama)
// generally follow the contract, but variance has been observed:
//   - Some providers emit arguments as an OBJECT directly
//     (`"arguments": {"x": 1}`). Caught here by the first-byte
//     check: raw[0] == '"' means string-encoded; anything else
//     (`{`, `[`, `n` for null) is returned unchanged so
//     CanonicalToolArg sees the value as-is.
//   - Some providers emit pretty-printed strings with leading
//     whitespace inside the encoded value. Surviving the
//     `json.Unmarshal(&s)` decode covers that case; the inner
//     spaces are preserved in the returned RawMessage and the
//     downstream tool-specific parser (Read/Grep/Bash) ignores
//     them via their own json.Unmarshal calls.
//
// If a provider ever sends arguments in some third shape (a
// base64-encoded payload, a typed wrapper object, etc.), this
// function silently returns the wrong canonical key for those
// requests. The repeat_reads counter would under-report rather
// than over-report — same logical input emits a different hash —
// which is the safer failure direction. Worth revisiting if Phase
// 1a telemetry shows a meaningful share of tool calls where
// repeat_reads fails to detect re-reads on a non-OpenAI upstream.
func unwrapOpenAIToolArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || raw[0] != '"' {
		return raw
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	return json.RawMessage(s)
}
