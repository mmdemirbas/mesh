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

// handleO2AStream handles Direction B streaming: client expects OpenAI SSE,
// upstream returns Anthropic SSE. Reads Anthropic SSE events from upstream and
// translates each to OpenAI SSE chunks.
func handleO2AStream(w http.ResponseWriter, r *http.Request, anthReq *MessagesRequest, cfg *GatewayCfg, client *http.Client, apiKey, clientModel string, oaiReq *ChatCompletionRequest, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	anthBody, _ := json.Marshal(anthReq)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", cfg.Upstream, bytes.NewReader(anthBody))
	if err != nil {
		writeOpenAIError(w, 500, "cannot create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		upstreamReq.Header.Set("x-api-key", apiKey)
		upstreamReq.Header.Set("anthropic-version", "2023-06-01")
	}

	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		writeOpenAIError(w, 502, "upstream request failed: "+err.Error())
		log.Error("Upstream stream request failed", "error", err)
		return
	}
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode != http.StatusOK {
		body := make([]byte, 4096)
		n, _ := upstreamResp.Body.Read(body)
		status := translateUpstreamErrorStatus(upstreamResp.StatusCode, cfg.Mode)
		writeOpenAIError(w, status, string(body[:n]))
		return
	}

	// Set SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, 500, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	includeUsage := oaiReq.StreamOptions != nil && oaiReq.StreamOptions.IncludeUsage

	st := &o2aStreamState{
		clientModel:  clientModel,
		w:            w,
		flusher:      flusher,
		metrics:      metrics,
		includeUsage: includeUsage,
	}

	scanner := bufio.NewScanner(upstreamResp.Body)
	var eventType string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event AnthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			log.Warn("Cannot parse Anthropic SSE event", "error", err, "event_type", eventType)
			continue
		}

		st.processEvent(eventType, &event)
	}

	// Finalize.
	st.finalize()

	metrics.Streams.Add(1)
	log.Info("Stream completed", "model", clientModel, "elapsed", time.Since(start))
}

// o2aStreamState tracks state during Anthropic->OpenAI SSE translation.
type o2aStreamState struct {
	clientModel  string
	w            http.ResponseWriter
	flusher      http.Flusher
	metrics      *state.Metrics
	includeUsage bool

	toolIndex    int
	sentFirst    bool
	finishReason string
	usage        *OpenAIUsage
	messageID    string
}

func (s *o2aStreamState) processEvent(eventType string, event *AnthropicStreamEvent) {
	switch eventType {
	case "message_start":
		if event.Message != nil {
			s.messageID = event.Message.ID
		}
		// Emit first chunk with role.
		s.emitChunk(OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Role: "assistant", Content: strPtr("")},
		}, nil)
		s.sentFirst = true

	case "content_block_start":
		if event.ContentBlock != nil {
			switch event.ContentBlock.Type {
			case "tool_use":
				// Emit tool call start.
				s.emitChunk(OpenAIChunkChoice{
					Index: 0,
					Delta: OpenAIChunkDelta{
						ToolCalls: []OpenAIChunkToolCall{{
							Index: s.toolIndex,
							ID:    event.ContentBlock.ID,
							Type:  "function",
							Function: OpenAIChunkFuncCall{
								Name:      event.ContentBlock.Name,
								Arguments: "",
							},
						}},
					},
				}, nil)
			case "thinking":
				// Drop.
			}
			// text block start: no output needed for OpenAI.
		}

	case "content_block_delta":
		if event.Delta == nil {
			return
		}
		switch event.Delta.Type {
		case "text_delta":
			s.emitChunk(OpenAIChunkChoice{
				Index: 0,
				Delta: OpenAIChunkDelta{Content: &event.Delta.Text},
			}, nil)

		case "input_json_delta":
			s.emitChunk(OpenAIChunkChoice{
				Index: 0,
				Delta: OpenAIChunkDelta{
					ToolCalls: []OpenAIChunkToolCall{{
						Index: s.toolIndex,
						Function: OpenAIChunkFuncCall{
							Arguments: event.Delta.PartialJSON,
						},
					}},
				},
			}, nil)
		}

	case "content_block_stop":
		// Check if the previous block was a tool — increment index.
		s.toolIndex++

	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			s.finishReason = mapAnthropicStopReason(event.Delta.StopReason)
		}
		if event.Usage != nil {
			s.usage = &OpenAIUsage{
				PromptTokens:     event.Usage.InputTokens,
				CompletionTokens: event.Usage.OutputTokens,
				TotalTokens:      event.Usage.InputTokens + event.Usage.OutputTokens,
			}
		}

	case "message_stop":
		// Handled in finalize.

	case "ping":
		// Ignore.

	case "error":
		// Mid-stream error. Emit as content and close.
		errMsg := "upstream error"
		if event.Delta != nil && event.Delta.Text != "" {
			errMsg = event.Delta.Text
		}
		s.emitChunk(OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: &errMsg},
		}, nil)
	}
}

func (s *o2aStreamState) finalize() {
	if s.finishReason == "" {
		s.finishReason = "stop"
	}

	// Emit final chunk with finish_reason.
	s.emitChunk(OpenAIChunkChoice{
		Index:        0,
		Delta:        OpenAIChunkDelta{},
		FinishReason: &s.finishReason,
	}, nil)

	// Emit usage chunk if requested.
	if s.includeUsage && s.usage != nil {
		s.emitChunk(OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{},
		}, s.usage)
	}

	// Emit [DONE].
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

func (s *o2aStreamState) emitChunk(choice OpenAIChunkChoice, usage *OpenAIUsage) {
	id := s.messageID
	if id == "" {
		id = "chatcmpl-stream"
	}
	chunk := ChatCompletionChunk{
		ID:      ensurePrefix(id, "chatcmpl-"),
		Object:  "chat.completion.chunk",
		Created: nowUnix(),
		Model:   s.clientModel,
		Choices: []OpenAIChunkChoice{choice},
		Usage:   usage,
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flusher.Flush()
	s.metrics.BytesTx.Add(int64(len(b) + 8))
}

func strPtr(s string) *string { return &s }
