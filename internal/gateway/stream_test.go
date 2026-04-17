package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- A2O Streaming Tests (client=Anthropic, upstream=OpenAI) ---

func TestA2OStream_SimpleText(t *testing.T) {
	t.Parallel()
	// Mock OpenAI streaming upstream.
	chunks := []string{
		makeOpenAIChunk("chatcmpl-1", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant", Content: strPtr("")},
		}),
		makeOpenAIChunk("chatcmpl-1", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr("Hello")},
		}),
		makeOpenAIChunk("chatcmpl-1", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr(" world")},
		}),
		makeOpenAIChunkWithFinish("chatcmpl-1", "gpt-4o", "stop", &OpenAIUsage{
			PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7,
		}),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", map[string]string{"claude-sonnet-4-6": "gpt-4o"})

	// Verify event sequence.
	assertEventType(t, events, 0, "message_start")
	assertContainsEventType(t, events, "content_block_start")
	assertContainsEventType(t, events, "content_block_delta")
	assertContainsEventType(t, events, "content_block_stop")
	assertContainsEventType(t, events, "message_delta")
	assertContainsEventType(t, events, "message_stop")

	// Verify text content.
	var textParts []string
	for _, e := range events {
		if e.eventType == "content_block_delta" {
			var data struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.Delta.Type == "text_delta" {
				textParts = append(textParts, data.Delta.Text)
			}
		}
	}
	fullText := strings.Join(textParts, "")
	if fullText != "Hello world" {
		t.Errorf("text = %q, want 'Hello world'", fullText)
	}

	// Verify stop_reason in message_delta.
	for _, e := range events {
		if e.eventType == "message_delta" {
			var data struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.Delta.StopReason != "end_turn" {
				t.Errorf("stop_reason = %q, want end_turn", data.Delta.StopReason)
			}
		}
	}
}

func TestA2OStream_ToolCall(t *testing.T) {
	t.Parallel()
	chunks := []string{
		makeOpenAIChunk("chatcmpl-2", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant"},
		}),
		// Tool call start: id + name.
		makeOpenAIChunkRaw("chatcmpl-2", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}`),
		// Arguments.
		makeOpenAIChunkRaw("chatcmpl-2", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}`),
		makeOpenAIChunkRaw("chatcmpl-2", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp\"}"}}]}}`),
		makeOpenAIChunkWithFinish("chatcmpl-2", "gpt-4o", "tool_calls", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	// Should have a tool_use content_block_start.
	var foundToolStart bool
	for _, e := range events {
		if e.eventType == "content_block_start" {
			var data struct {
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.ContentBlock.Type == "tool_use" {
				foundToolStart = true
				if data.ContentBlock.ID != "call_1" {
					t.Errorf("tool id = %q, want call_1", data.ContentBlock.ID)
				}
				if data.ContentBlock.Name != "read_file" {
					t.Errorf("tool name = %q, want read_file", data.ContentBlock.Name)
				}
			}
		}
	}
	if !foundToolStart {
		t.Error("no tool_use content_block_start found")
	}

	// Verify stop_reason is tool_use.
	for _, e := range events {
		if e.eventType == "message_delta" {
			var data struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.Delta.StopReason != "tool_use" {
				t.Errorf("stop_reason = %q, want tool_use", data.Delta.StopReason)
			}
		}
	}
}

func TestA2OStream_EmptyToolCallsArray(t *testing.T) {
	t.Parallel()
	// mlx_lm.server edge case: always sends tool_calls: [].
	chunks := []string{
		makeOpenAIChunk("chatcmpl-3", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant", Content: strPtr("")},
		}),
		// Empty tool_calls with text content.
		makeOpenAIChunkRaw("chatcmpl-3", "gpt-4o", `{"index":0,"delta":{"content":"Hello","tool_calls":[]}}`),
		makeOpenAIChunkWithFinish("chatcmpl-3", "gpt-4o", "stop", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	// Should NOT have any tool_use blocks.
	for _, e := range events {
		if e.eventType == "content_block_start" {
			var data struct {
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.ContentBlock.Type == "tool_use" {
				t.Error("should not have tool_use block from empty tool_calls array")
			}
		}
	}
}

// --- O2A Streaming Tests (client=OpenAI, upstream=Anthropic) ---

func TestO2AStream_SimpleText(t *testing.T) {
	t.Parallel()
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", map[string]string{"gpt-4o": "claude-sonnet-4-6"}, true)

	// Verify we get text content deltas.
	var textParts []string
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil {
			textParts = append(textParts, *chunk.Choices[0].Delta.Content)
		}
	}

	fullText := strings.Join(textParts, "")
	// First chunk has empty content, then "Hello", then " world".
	if !strings.Contains(fullText, "Hello world") {
		t.Errorf("text = %q, want to contain 'Hello world'", fullText)
	}

	// Verify finish_reason.
	var foundFinish bool
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			if *chunk.Choices[0].FinishReason != "stop" {
				t.Errorf("finish_reason = %q, want stop", *chunk.Choices[0].FinishReason)
			}
			foundFinish = true
		}
	}
	if !foundFinish {
		t.Error("no finish_reason found")
	}

	// Verify [DONE].
	lastEvent := events[len(events)-1]
	if lastEvent.data != "[DONE]" {
		t.Errorf("last event data = %q, want [DONE]", lastEvent.data)
	}
}

func TestO2AStream_ToolCall(t *testing.T) {
	t.Parallel()
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read_file","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/tmp\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":20}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", nil, false)

	// Verify tool call chunks.
	var foundToolStart bool
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			tc := chunk.Choices[0].Delta.ToolCalls[0]
			if tc.ID == "call_1" {
				foundToolStart = true
				if tc.Function.Name != "read_file" {
					t.Errorf("tool name = %q, want read_file", tc.Function.Name)
				}
			}
		}
	}
	if !foundToolStart {
		t.Error("no tool call start found")
	}

	// Verify finish_reason is tool_calls.
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			if *chunk.Choices[0].FinishReason != "tool_calls" {
				t.Errorf("finish_reason = %q, want tool_calls", *chunk.Choices[0].FinishReason)
			}
		}
	}
}

func TestO2AStream_ThinkingWrapped(t *testing.T) {
	t.Parallel()
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":3}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", nil, false)

	// Thinking should appear as <think>...</think> tags in text content.
	var allText strings.Builder
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil {
			allText.WriteString(*chunk.Choices[0].Delta.Content)
		}
	}
	got := allText.String()
	if !strings.Contains(got, "<think>") || !strings.Contains(got, "</think>") {
		t.Errorf("expected <think> wrapper in output, got %q", got)
	}
	if !strings.Contains(got, "Let me think...") {
		t.Errorf("expected thinking content in output, got %q", got)
	}
	if !strings.Contains(got, "Hello") {
		t.Errorf("expected 'Hello' text in output, got %q", got)
	}
}

func TestA2OStream_ThinkTagTranslated(t *testing.T) {
	t.Parallel()
	// Upstream returns text with <think>...</think> wrapper (common with
	// OpenAI-compatible endpoints proxying Claude).
	chunks := []string{
		makeOpenAIChunk("chatcmpl-5", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant", Content: strPtr("")},
		}),
		makeOpenAIChunk("chatcmpl-5", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr("<think>")},
		}),
		makeOpenAIChunk("chatcmpl-5", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr("I should greet them")},
		}),
		makeOpenAIChunk("chatcmpl-5", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr("</think>\n\nHello!")},
		}),
		makeOpenAIChunkWithFinish("chatcmpl-5", "gpt-4o", "stop", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	// Should get native thinking block + text "Hello!".
	var allText, allThinking strings.Builder
	var hasThinkingBlock bool
	for _, e := range events {
		if e.eventType == "content_block_start" {
			var data struct {
				ContentBlock struct{ Type string } `json:"content_block"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.ContentBlock.Type == "thinking" {
				hasThinkingBlock = true
			}
		}
		if e.eventType != "content_block_delta" {
			continue
		}
		var data struct {
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(e.data), &data); err != nil {
			continue
		}
		switch data.Delta.Type {
		case "text_delta":
			allText.WriteString(data.Delta.Text)
		case "thinking_delta":
			allThinking.WriteString(data.Delta.Thinking)
		}
	}
	if !hasThinkingBlock {
		t.Error("expected a thinking content_block_start event")
	}
	if !strings.Contains(allThinking.String(), "greet") {
		t.Errorf("thinking = %q, expected to contain 'greet'", allThinking.String())
	}
	if !strings.Contains(allText.String(), "Hello!") {
		t.Errorf("text = %q, expected 'Hello!'", allText.String())
	}
	if strings.Contains(allText.String(), "think") {
		t.Errorf("thinking leaked into text: %q", allText.String())
	}
}

func TestA2OStream_NoThinkTag(t *testing.T) {
	t.Parallel()
	// Text that does NOT start with <think> should pass through unchanged.
	chunks := []string{
		makeOpenAIChunk("chatcmpl-6", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant", Content: strPtr("")},
		}),
		makeOpenAIChunk("chatcmpl-6", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: strPtr("Just a normal response")},
		}),
		makeOpenAIChunkWithFinish("chatcmpl-6", "gpt-4o", "stop", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	var allText strings.Builder
	for _, e := range events {
		if e.eventType != "content_block_delta" {
			continue
		}
		var data struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(e.data), &data); err != nil {
			continue
		}
		if data.Delta.Type == "text_delta" {
			allText.WriteString(data.Delta.Text)
		}
	}
	if got := allText.String(); got != "Just a normal response" {
		t.Errorf("text = %q, want 'Just a normal response'", got)
	}
}

func TestA2OStream_ToolNameAndArgsInSameChunk(t *testing.T) {
	t.Parallel()
	// Edge case: tool name + arguments arrive in the same SSE chunk.
	chunks := []string{
		makeOpenAIChunk("chatcmpl-4", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant"},
		}),
		// Name AND arguments in the same chunk.
		makeOpenAIChunkRaw("chatcmpl-4", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}`),
		makeOpenAIChunkWithFinish("chatcmpl-4", "gpt-4o", "tool_calls", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	// Should have both content_block_start AND content_block_delta for the tool.
	var hasStart, hasDelta bool
	for _, e := range events {
		if e.eventType == "content_block_start" {
			var data struct {
				ContentBlock struct{ Type string } `json:"content_block"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.ContentBlock.Type == "tool_use" {
				hasStart = true
			}
		}
		if e.eventType == "content_block_delta" {
			var data struct {
				Delta struct{ Type string } `json:"delta"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.Delta.Type == "input_json_delta" {
				hasDelta = true
			}
		}
	}
	if !hasStart {
		t.Error("no content_block_start for tool_use")
	}
	if !hasDelta {
		t.Error("no input_json_delta from same chunk as tool name")
	}
}

func TestA2OStream_ParallelToolCalls(t *testing.T) {
	t.Parallel()
	chunks := []string{
		makeOpenAIChunk("chatcmpl-5", "gpt-4o", &OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant"},
		}),
		// First tool call.
		makeOpenAIChunkRaw("chatcmpl-5", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]}}`),
		// Second tool call.
		makeOpenAIChunkRaw("chatcmpl-5", "gpt-4o", `{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"write","arguments":"{}"}}]}}`),
		makeOpenAIChunkWithFinish("chatcmpl-5", "gpt-4o", "tool_calls", nil),
	}

	upstream := makeSSEServer(chunks)
	defer upstream.Close()

	events := runA2OStreamTest(t, upstream.URL, "claude-sonnet-4-6", nil)

	var toolStarts []string
	for _, e := range events {
		if e.eventType == "content_block_start" {
			var data struct {
				ContentBlock struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			json.Unmarshal([]byte(e.data), &data)
			if data.ContentBlock.Type == "tool_use" {
				toolStarts = append(toolStarts, data.ContentBlock.Name)
			}
		}
	}
	if len(toolStarts) != 2 {
		t.Fatalf("tool starts = %d, want 2", len(toolStarts))
	}
	if toolStarts[0] != "read" || toolStarts[1] != "write" {
		t.Errorf("tool names = %v, want [read, write]", toolStarts)
	}
}

func TestO2AStream_ParallelToolCalls(t *testing.T) {
	t.Parallel()
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_p","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_a","name":"read","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_b","name":"write","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":20}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", nil, false)

	// Collect tool call starts with their indices.
	type toolStart struct {
		index int
		name  string
	}
	var starts []toolStart
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				if tc.ID != "" {
					starts = append(starts, toolStart{index: tc.Index, name: tc.Function.Name})
				}
			}
		}
	}
	if len(starts) != 2 {
		t.Fatalf("tool starts = %d, want 2", len(starts))
	}
	if starts[0].index != 0 || starts[0].name != "read" {
		t.Errorf("tool[0] = %+v, want {0, read}", starts[0])
	}
	if starts[1].index != 1 || starts[1].name != "write" {
		t.Errorf("tool[1] = %+v, want {1, write}", starts[1])
	}
}

func TestO2AStream_MidStreamError(t *testing.T) {
	t.Parallel()
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_e","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`event: error` + "\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"server is overloaded"}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", nil, false)

	// Should contain text "Hello" + error message content + [DONE].
	var hasError bool
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil {
			if *chunk.Choices[0].Delta.Content == "server is overloaded" {
				hasError = true
			}
		}
	}
	if !hasError {
		t.Error("mid-stream error message not found in output")
	}

	// Should end with [DONE].
	lastEvent := events[len(events)-1]
	if lastEvent.data != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", lastEvent.data)
	}
}

func TestO2AStream_MixedTextAndTool(t *testing.T) {
	t.Parallel()
	// Text block followed by tool block — verifies toolIndex is correct.
	anthropicEvents := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_m","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me read that."}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read_file","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":10}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}

	upstream := makeAnthropicSSEServer(anthropicEvents)
	defer upstream.Close()

	events := runO2AStreamTest(t, upstream.URL, "gpt-4o", nil, false)

	// The tool call should have index 0 (first tool, text blocks don't count).
	for _, e := range events {
		if e.data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				if tc.ID == "call_1" && tc.Index != 0 {
					t.Errorf("tool index = %d, want 0 (text block stop should not increment)", tc.Index)
				}
			}
		}
	}
}

// --- Test Helpers ---

type sseEvent struct {
	eventType string
	data      string
}

func runA2OStreamTest(t *testing.T, upstreamURL, model string, modelMap map[string]string) []sseEvent {
	t.Helper()

	cfg := GatewayCfg{
		Name:        "test-a2o-stream",
		Bind:        "127.0.0.1:0",
		ClientAPI:   APIAnthropic,
		UpstreamAPI: APIOpenAI,
		Upstream:    upstreamURL,
		ModelMap:    modelMap,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	reqBody := fmt.Sprintf(`{"model":%q,"max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, model)
	resp, err := http.Post("http://"+cfg.Bind+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	return parseAnthropicSSE(t, resp)
}

func runO2AStreamTest(t *testing.T, upstreamURL, model string, modelMap map[string]string, includeUsage bool) []sseEvent {
	t.Helper()

	cfg := GatewayCfg{
		Name:        "test-o2a-stream",
		Bind:        "127.0.0.1:0",
		ClientAPI:   APIOpenAI,
		UpstreamAPI: APIAnthropic,
		Upstream:    upstreamURL,
		ModelMap:    modelMap,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = ln.Addr().String()
	ln.Close()

	go func() {
		Start(ctx, cfg, slog.Default()) //nolint:errcheck
	}()

	waitForHTTP(t, "http://"+cfg.Bind+"/health", 2*time.Second)

	streamOpts := ""
	if includeUsage {
		streamOpts = `,"stream_options":{"include_usage":true}`
	}
	reqBody := fmt.Sprintf(`{"model":%q,"max_tokens":1024,"stream":true%s,"messages":[{"role":"user","content":"Hi"}]}`, model, streamOpts)
	resp, err := http.Post("http://"+cfg.Bind+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	return parseOpenAISSE(t, resp)
}

func parseAnthropicSSE(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			events = append(events, sseEvent{eventType: currentEvent, data: data})
		}
	}
	return events
}

func parseOpenAISSE(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			events = append(events, sseEvent{data: data})
		}
	}
	return events
}

func assertEventType(t *testing.T, events []sseEvent, index int, eventType string) {
	t.Helper()
	if index >= len(events) {
		t.Fatalf("event[%d]: out of range (len=%d)", index, len(events))
	}
	if events[index].eventType != eventType {
		t.Errorf("event[%d].type = %q, want %q", index, events[index].eventType, eventType)
	}
}

func assertContainsEventType(t *testing.T, events []sseEvent, eventType string) {
	t.Helper()
	for _, e := range events {
		if e.eventType == eventType {
			return
		}
	}
	t.Errorf("no event with type %q found", eventType)
}

// makeSSEServer creates a test server that returns OpenAI-format SSE.
func makeSSEServer(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)

		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// makeAnthropicSSEServer creates a test server that returns Anthropic-format SSE.
func makeAnthropicSSEServer(events []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)

		for _, event := range events {
			fmt.Fprint(w, event+"\n\n")
			flusher.Flush()
		}
	}))
}

func makeOpenAIChunk(id, model string, choice *OpenAIChunkChoice) string {
	chunk := ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChunkChoice{*choice},
	}
	b, _ := json.Marshal(chunk)
	return string(b)
}

func makeOpenAIChunkRaw(id, model, choiceJSON string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[%s]}`,
		id, time.Now().Unix(), model, choiceJSON)
}

func makeOpenAIChunkWithFinish(id, model, reason string, usage *OpenAIUsage) string {
	chunk := ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChunkChoice{{
			Index:        0,
			Delta:        OpenAIChunkDelta{},
			FinishReason: &reason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(chunk)
	return string(b)
}
