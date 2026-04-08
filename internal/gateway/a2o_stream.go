package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

// handleA2OStream handles Direction A streaming: client expects Anthropic SSE,
// upstream returns OpenAI SSE. Reads OpenAI SSE chunks from upstream and
// translates each to Anthropic SSE events.
func handleA2OStream(w http.ResponseWriter, r *http.Request, oaiReq *ChatCompletionRequest, cfg *GatewayCfg, client *http.Client, apiKey, clientModel string, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	oaiBody, _ := json.Marshal(oaiReq)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", cfg.Upstream, bytes.NewReader(oaiBody))
	if err != nil {
		writeAnthropicError(w, 500, "cannot create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		writeAnthropicError(w, 502, "upstream request failed: "+err.Error())
		log.Error("Upstream stream request failed", "error", err)
		return
	}
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode != http.StatusOK {
		body := make([]byte, 4096)
		n, _ := upstreamResp.Body.Read(body)
		status := translateUpstreamErrorStatus(upstreamResp.StatusCode, cfg.Mode)
		writeAnthropicError(w, status, string(body[:n]))
		return
	}

	// Set SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, 500, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	st := &a2oStreamState{
		clientModel: clientModel,
		w:           w,
		flusher:     flusher,
		metrics:     metrics,
	}

	// Emit message_start.
	st.emitMessageStart()

	scanner := bufio.NewScanner(upstreamResp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			log.Warn("Cannot parse SSE chunk", "error", err)
			continue
		}

		st.processChunk(&chunk)
	}

	// Finalize.
	st.finalize()

	metrics.Streams.Add(1)
	log.Info("Stream completed", "model", clientModel, "elapsed", time.Since(start))
}

// a2oStreamState tracks state during OpenAI->Anthropic SSE translation.
type a2oStreamState struct {
	clientModel string
	w           http.ResponseWriter
	flusher     http.Flusher
	metrics     *state.Metrics

	blockIndex    int
	inTextBlock   bool
	inToolBlock   bool
	lastToolIndex int
	usage         AnthropicUsage
	stopReason    string
	hasBlock      bool
}

func (s *a2oStreamState) processChunk(chunk *ChatCompletionChunk) {
	if len(chunk.Choices) == 0 {
		// Usage-only chunk (trailing chunk with include_usage).
		if chunk.Usage != nil {
			s.usage = AnthropicUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		return
	}

	choice := chunk.Choices[0]

	// Capture finish_reason.
	if choice.FinishReason != nil {
		s.stopReason = mapOpenAIFinishReason(*choice.FinishReason)
	}

	// Capture usage if present.
	if chunk.Usage != nil {
		s.usage = AnthropicUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
		}
	}

	// Text content delta.
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		if !s.inTextBlock {
			s.startTextBlock()
		}
		s.emitTextDelta(*choice.Delta.Content)
	}

	// Tool call deltas.
	if len(choice.Delta.ToolCalls) > 0 {
		for _, tc := range choice.Delta.ToolCalls {
			// Skip empty tool_calls arrays (mlx_lm.server edge case).
			if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
				continue
			}

			if tc.ID != "" {
				// New tool call — close previous block.
				s.closeCurrentBlock()
				s.startToolBlock(tc.ID, tc.Function.Name)
				s.lastToolIndex = tc.Index
			}

			// Argument delta.
			if tc.Function.Arguments != "" {
				s.emitInputJSONDelta(tc.Function.Arguments)
			}
		}
	}
}

func (s *a2oStreamState) startTextBlock() {
	s.closeCurrentBlock()
	s.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	s.inTextBlock = true
	s.hasBlock = true
}

func (s *a2oStreamState) emitTextDelta(text string) {
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *a2oStreamState) startToolBlock(id, name string) {
	s.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
	s.inToolBlock = true
	s.hasBlock = true
}

func (s *a2oStreamState) emitInputJSONDelta(partial string) {
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
}

func (s *a2oStreamState) closeCurrentBlock() {
	if s.inTextBlock || s.inToolBlock {
		s.emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": s.blockIndex,
		})
		s.blockIndex++
		s.inTextBlock = false
		s.inToolBlock = false
	}
}

func (s *a2oStreamState) emitMessageStart() {
	s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      "msg_stream",
			"type":    "message",
			"role":    "assistant",
			"model":   s.clientModel,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *a2oStreamState) finalize() {
	s.closeCurrentBlock()

	if s.stopReason == "" {
		s.stopReason = "end_turn"
	}

	s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stopReason},
		"usage": map[string]any{
			"input_tokens":  s.usage.InputTokens,
			"output_tokens": s.usage.OutputTokens,
		},
	})

	s.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (s *a2oStreamState) emit(event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	s.flusher.Flush()
	s.metrics.BytesTx.Add(int64(len(b) + len(event) + 20))
}
