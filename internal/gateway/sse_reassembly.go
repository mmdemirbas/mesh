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
}

// SSEToolCall aggregates a single tool invocation across its streamed deltas.
type SSEToolCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// reassembleSSE parses raw SSE bytes from the given upstream format and
// returns a compact summary. Returns nil only when body is empty; otherwise
// the summary is populated even if parsing is partial (client cancel, mid-
// stream error). Event boundaries are blank lines; within an event, `event:`
// and `data:` lines are combined.
func reassembleSSE(body []byte, upstreamAPI string) *SSESummary {
	if len(body) == 0 {
		return nil
	}
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
			case "thinking_delta":
				thinking.WriteString(ev.Delta.Thinking)
			case "input_json_delta":
				if b, ok := toolArgs[ev.Index]; ok {
					b.WriteString(ev.Delta.PartialJSON)
				}
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
		case "error":
			if ev.Error != nil {
				s.Errors = append(s.Errors, ev.Error.Type+": "+ev.Error.Message)
			}
		}
	})

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
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
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
		PromptTokens            int `json:"prompt_tokens,omitempty"`
		CompletionTokens        int `json:"completion_tokens,omitempty"`
		PromptTokensDetails     *struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

func reassembleOpenAISSE(body []byte) *SSESummary {
	s := &SSESummary{}
	var (
		content  strings.Builder
		toolBuf  = map[int]*SSEToolCall{}
		toolArgs = map[int]*strings.Builder{}
	)

	iterEvents(body, func(_ string, data string) {
		if data == "" || data == "[DONE]" {
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
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					_, _ = toolArgs[tc.Index].WriteString(tc.Function.Arguments)
				}
			}
			if ci.FinishReason != nil && *ci.FinishReason != "" {
				s.StopReason = *ci.FinishReason
			}
		}
	})

	s.Content = content.String()
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
	return s
}
