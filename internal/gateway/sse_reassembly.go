package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// SSESummary is the compact, human-readable view of a streamed response,
// reassembled from raw SSE bytes. Fields are omitted when empty so the JSONL
// row stays tidy.
type SSESummary struct {
	// Number of SSE events parsed (data: lines).
	Events int `json:"events"`
	// Concatenated assistant text from all content_block_delta / delta.content.
	Content string `json:"content,omitempty"`
	// Concatenated thinking text (Anthropic extended-thinking blocks).
	Thinking string `json:"thinking,omitempty"`
	// Tool-use calls with arguments reassembled from partial JSON deltas.
	ToolCalls []SSEToolCall `json:"tool_calls,omitempty"`
	// Stop reason reported by the upstream (Anthropic message_delta.stop_reason
	// or mapped OpenAI finish_reason).
	StopReason string `json:"stop_reason,omitempty"`
	// Token accounting captured from the stream (when provided).
	Usage *Usage `json:"usage,omitempty"`
	// Mid-stream error events (Anthropic event: error).
	Errors []string `json:"errors,omitempty"`
	// Upstream-assigned message id (Anthropic message_start.message.id /
	// OpenAI chunk id).
	MessageID string `json:"message_id,omitempty"`
	// Model name reported by upstream.
	Model string `json:"model,omitempty"`

	// ResponseBytes is the §4.3 partition of the decoded response body.
	// Populated by the reassembler; copied into ResponseMeta separately
	// by wrapAuditing so it lands as a top-level audit row field rather
	// than nested under stream_summary. json:"-" prevents that double
	// emission.
	ResponseBytes *ResponseByteCounts `json:"-"`
	// Terminated is the §4.3 termination state observed by the
	// reassembler — either "normal" (terminal marker seen) or
	// "upstream" (no terminal marker). wrapAuditing flips this to
	// "client" when context cancellation took precedence over the
	// missing terminal marker. json:"-" mirrors ResponseBytes.
	Terminated string `json:"-"`
}

// ResponseByteCounts is the §4.3 partition of a response body's bytes.
// Sums to len(decoded body) exactly: Other absorbs framing scaffolding
// (`event:`, `data:`, `\n\n`, `[DONE]`), block-start/stop wrappers,
// metadata events, and any non-payload fields. Same I1/I2 invariants
// as SectionByteCounts on the request side.
//
// Counting convention mirrors §4.1: payload string lengths only —
// JSON quoting and structural envelope go to Other.
type ResponseByteCounts struct {
	Text      int `json:"text"`
	Thinking  int `json:"thinking"`
	ToolCalls int `json:"tool_calls"`
	Other     int `json:"other"`
	Total     int `json:"total"`
}

// SSEToolCall aggregates a single tool invocation across its streamed deltas.
type SSEToolCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// reassembleSSE parses raw SSE bytes from the given upstream format and
// returns a compact summary. Always returns a non-nil result for known
// APIs so callers can rely on the §4.3 partition closure invariant
// even for empty / malformed / truncated streams. The empty-body case
// (upstream connected, sent nothing, closed) yields an SSESummary
// with Events=0, ResponseBytes.Total=0, Terminated="upstream".
//
// Event boundaries are blank lines; within an event, `event:` and
// `data:` lines are combined. Returns nil only when upstreamAPI is
// neither APIAnthropic nor APIOpenAI — that branch is a programmer
// error, not data drift, so the caller should not see it in
// production.
func reassembleSSE(body []byte, upstreamAPI string) *SSESummary {
	switch upstreamAPI {
	case APIAnthropic:
		return reassembleAnthropicSSE(body)
	case APIOpenAI:
		return reassembleOpenAISSE(body)
	}
	return nil
}

// iterEvents yields (eventName, dataPayload) pairs from raw SSE bytes.
// `eventName` is empty when the event had no explicit `event:` line.
func iterEvents(body []byte, yield func(event string, data string)) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var eventName string
	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 && eventName == "" {
			return
		}
		yield(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	// No trailing blank line is common when the stream was cut — flush whatever
	// we have.
	flush()
}

// --- Anthropic ---

type anthropicEvent struct {
	Type    string           `json:"type"`
	Message *anthropicMsg    `json:"message,omitempty"`
	Index   int              `json:"index,omitempty"`
	Block   *anthropicBlock  `json:"content_block,omitempty"`
	Delta   *anthropicDelta  `json:"delta,omitempty"`
	Usage   *anthropicUsage  `json:"usage,omitempty"`
	Error   *anthropicErrObj `json:"error,omitempty"`
}

type anthropicMsg struct {
	ID    string          `json:"id,omitempty"`
	Model string          `json:"model,omitempty"`
	Usage *anthropicUsage `json:"usage,omitempty"`
}
type anthropicBlock struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}
type anthropicDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}
type anthropicErrObj struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

func reassembleAnthropicSSE(body []byte) *SSESummary {
	s := &SSESummary{}
	rb := &ResponseByteCounts{}
	var sawTerminal bool
	var (
		content  strings.Builder
		thinking strings.Builder
		toolBuf  = map[int]*SSEToolCall{} // index -> call
		toolArgs = map[int]*strings.Builder{}
	)

	iterEvents(body, func(_ string, data string) {
		if data == "" {
			return
		}
		s.Events++
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				s.MessageID = ev.Message.ID
				s.Model = ev.Message.Model
				if ev.Message.Usage != nil {
					s.Usage = &Usage{
						InputTokens:              ev.Message.Usage.InputTokens,
						OutputTokens:             ev.Message.Usage.OutputTokens,
						CacheCreationInputTokens: ev.Message.Usage.CacheCreationInputTokens,
						CacheReadInputTokens:     ev.Message.Usage.CacheReadInputTokens,
					}
				}
			}
		case "content_block_start":
			if ev.Block != nil && ev.Block.Type == "tool_use" {
				// Tool name + id from the block-start header are
				// the per-tool identifying payload; the
				// surrounding `{"type":"content_block_start"...}`
				// JSON wrapper is scaffolding (Other).
				rb.ToolCalls += len(ev.Block.Name) + len(ev.Block.ID)
				toolBuf[ev.Index] = &SSEToolCall{ID: ev.Block.ID, Name: ev.Block.Name}
				toolArgs[ev.Index] = &strings.Builder{}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				break
			}
			switch ev.Delta.Type {
			case "text_delta":
				content.WriteString(ev.Delta.Text)
				rb.Text += len(ev.Delta.Text)
			case "thinking_delta":
				thinking.WriteString(ev.Delta.Thinking)
				rb.Thinking += len(ev.Delta.Thinking)
			case "input_json_delta":
				if b, ok := toolArgs[ev.Index]; ok {
					b.WriteString(ev.Delta.PartialJSON)
				}
				rb.ToolCalls += len(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			// No-op; buffers are finalized during summary assembly.
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				s.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				if s.Usage == nil {
					s.Usage = &Usage{}
				}
				if ev.Usage.OutputTokens != 0 {
					s.Usage.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens != 0 {
					s.Usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheCreationInputTokens != 0 {
					s.Usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
				}
				if ev.Usage.CacheReadInputTokens != 0 {
					s.Usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
			}
		case "message_stop":
			sawTerminal = true
		case "error":
			if ev.Error != nil {
				s.Errors = append(s.Errors, ev.Error.Type+": "+ev.Error.Message)
			}
		}
	})

	rb.Total = len(body)
	rb.Other = rb.Total - rb.Text - rb.Thinking - rb.ToolCalls
	if rb.Other < 0 {
		// Defensive: a named section over-counted relative to the wire
		// body. Force partition closure by clamping Other to 0; the
		// drift would surface in the test that asserts I3 (named ≤
		// total). Logging would be ideal but the reassembler doesn't
		// hold a logger today.
		rb.Other = 0
	}
	s.ResponseBytes = rb
	if sawTerminal {
		s.Terminated = "normal"
	} else {
		s.Terminated = "upstream"
	}

	s.Content = content.String()
	s.Thinking = thinking.String()
	if len(toolBuf) > 0 {
		// Stable order by index.
		for i := range 1024 {
			call, ok := toolBuf[i]
			if !ok {
				continue
			}
			if b, ok := toolArgs[i]; ok && b.Len() > 0 {
				raw := b.String()
				if json.Valid([]byte(raw)) {
					call.Args = json.RawMessage(raw)
				} else {
					// Fall back to raw string when partial JSON was truncated.
					marshalled, _ := json.Marshal(raw)
					call.Args = json.RawMessage(marshalled)
				}
			}
			s.ToolCalls = append(s.ToolCalls, *call)
		}
	}
	return s
}

// --- OpenAI ---

type openaiChunk struct {
	ID      string `json:"id,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Delta struct {
			Content string `json:"content,omitempty"`
			// ReasoningContent is the panshi / DeepSeek R1 / Qwen QwQ
			// extension: streamed model reasoning that is NOT part of
			// the user-facing answer. Counted under thinking per
			// SPEC §4.3 — same semantic role as Anthropic
			// thinking_delta.
			ReasoningContent string `json:"reasoning_content,omitempty"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason,omitempty"`
	} `json:"choices,omitempty"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens,omitempty"`
		CompletionTokens    int `json:"completion_tokens,omitempty"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

func reassembleOpenAISSE(body []byte) *SSESummary {
	s := &SSESummary{}
	rb := &ResponseByteCounts{}
	var sawTerminal bool
	var (
		content  strings.Builder
		thinking strings.Builder
		toolBuf  = map[int]*SSEToolCall{}
		toolArgs = map[int]*strings.Builder{}
	)

	iterEvents(body, func(_ string, data string) {
		if data == "" {
			return
		}
		if data == "[DONE]" {
			sawTerminal = true
			return
		}
		s.Events++
		var ch openaiChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return
		}
		if ch.ID != "" && s.MessageID == "" {
			s.MessageID = ch.ID
		}
		if ch.Model != "" && s.Model == "" {
			s.Model = ch.Model
		}
		if ch.Usage != nil {
			u := &Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
			if ch.Usage.PromptTokensDetails != nil {
				u.CacheReadInputTokens = ch.Usage.PromptTokensDetails.CachedTokens
			}
			if ch.Usage.CompletionTokensDetails != nil {
				u.ReasoningTokens = ch.Usage.CompletionTokensDetails.ReasoningTokens
			}
			s.Usage = u
		}
		for _, ci := range ch.Choices {
			if ci.Delta.Content != "" {
				content.WriteString(ci.Delta.Content)
				rb.Text += len(ci.Delta.Content)
			}
			if ci.Delta.ReasoningContent != "" {
				thinking.WriteString(ci.Delta.ReasoningContent)
				rb.Thinking += len(ci.Delta.ReasoningContent)
			}
			for _, tc := range ci.Delta.ToolCalls {
				call, ok := toolBuf[tc.Index]
				if !ok {
					call = &SSEToolCall{}
					toolBuf[tc.Index] = call
					toolArgs[tc.Index] = &strings.Builder{}
				}
				if tc.ID != "" {
					call.ID = tc.ID
					rb.ToolCalls += len(tc.ID)
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
					rb.ToolCalls += len(tc.Function.Name)
				}
				if tc.Function.Arguments != "" {
					_, _ = toolArgs[tc.Index].WriteString(tc.Function.Arguments)
					rb.ToolCalls += len(tc.Function.Arguments)
				}
			}
			if ci.FinishReason != nil && *ci.FinishReason != "" {
				s.StopReason = *ci.FinishReason
			}
		}
	})

	s.Content = content.String()
	s.Thinking = thinking.String()
	if len(toolBuf) > 0 {
		for i := range 1024 {
			call, ok := toolBuf[i]
			if !ok {
				continue
			}
			if b, ok := toolArgs[i]; ok && b.Len() > 0 {
				raw := b.String()
				if json.Valid([]byte(raw)) {
					call.Args = json.RawMessage(raw)
				} else {
					marshalled, _ := json.Marshal(raw)
					call.Args = json.RawMessage(marshalled)
				}
			}
			s.ToolCalls = append(s.ToolCalls, *call)
		}
	}

	rb.Total = len(body)
	rb.Other = rb.Total - rb.Text - rb.Thinking - rb.ToolCalls
	if rb.Other < 0 {
		rb.Other = 0
	}
	s.ResponseBytes = rb
	if sawTerminal {
		s.Terminated = "normal"
	} else {
		s.Terminated = "upstream"
	}
	return s
}
