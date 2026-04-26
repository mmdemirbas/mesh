package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

// handleO2AStream handles Direction B streaming: client expects OpenAI SSE,
// upstream returns Anthropic SSE. Reads Anthropic SSE events from upstream and
// translates each to OpenAI SSE chunks.
func handleO2AStream(w http.ResponseWriter, r *http.Request, anthReq *MessagesRequest, upstream *ResolvedUpstream, clientModel string, oaiReq *ChatCompletionRequest, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	anthBody, _ := json.Marshal(anthReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = anthBody
	}

	ctx := r.Context()
	au := getAuditUpstream(r)
	sessionID := ""
	if au != nil {
		ctx = attachTimingTrace(ctx, au.Timer, au.ReqID)
		sessionID = au.SessionID
	}
	// deep-review I2: chain fallback for pre-stream errors. See
	// a2o_stream.go for the rationale (read from request context so
	// chain semantics work even without an audit recorder).
	chain := chainFromRequest(r)
	if len(chain) == 0 {
		chain = []*ResolvedUpstream{upstream}
	}
	var (
		streamUpstream *ResolvedUpstream
		streamKey      *KeyState
		attemptStart   time.Time
		upstreamResp   *http.Response
	)
	for chainIdx, up := range chain {
		key := up.Keys.Pick(ctx, RequestContext{Now: time.Now(), SessionID: sessionID})
		upstreamReq, err := http.NewRequestWithContext(ctx, "POST", up.Cfg.Target, bytes.NewReader(anthBody))
		if err != nil {
			writeOpenAIError(w, 500, "cannot create upstream request")
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		keyValue := ""
		switch {
		case key != nil && key.Value != "":
			keyValue = key.Value
		case up.APIKey != "":
			keyValue = up.APIKey
		}
		if keyValue != "" {
			hdr := map[string]string{}
			applyAuthHeaders(hdr, up.Cfg.API, keyValue)
			for k, v := range hdr {
				upstreamReq.Header.Set(k, v)
			}
		}

		stepStart := time.Now()
		resp, err := up.Client.Do(upstreamReq)
		if err != nil {
			recordStreamAttempt(au, up, key, stepStart, 0, nil, nil, err)
			if chainIdx < len(chain)-1 {
				if au != nil && len(au.Attempts) > 0 {
					au.Attempts[len(au.Attempts)-1].FallbackReason = "chain_advance:network_error"
				}
				log.Warn("streaming upstream network error, advancing chain", "upstream", up.Cfg.Name, "error", err)
				continue
			}
			writeOpenAIError(w, 502, "upstream request failed")
			log.Error("upstream stream request failed", "error", err)
			return
		}

		if resp.StatusCode == http.StatusOK {
			streamUpstream = up
			streamKey = key
			attemptStart = stepStart
			upstreamResp = resp
			break
		}

		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		recordStreamAttempt(au, up, key, stepStart, resp.StatusCode, resp.Header, errBody, nil)

		outcome := classifyOutcome(resp.StatusCode, nil)
		if !shouldAdvanceChain(outcome) {
			if au != nil {
				au.RespBody = errBody
			}
			status := translateUpstreamErrorStatus(resp.StatusCode, DirO2A)
			writeOpenAIError(w, status, translatedUpstreamErrorMessage(errBody))
			log.Warn("upstream stream client error", "status", resp.StatusCode, "body", string(errBody))
			return
		}
		if chainIdx >= len(chain)-1 {
			if au != nil {
				au.RespBody = errBody
			}
			status := translateUpstreamErrorStatus(resp.StatusCode, DirO2A)
			writeOpenAIError(w, status, translatedUpstreamErrorMessage(errBody))
			log.Warn("upstream stream error, chain exhausted", "status", resp.StatusCode, "body", string(errBody))
			return
		}
		if au != nil && len(au.Attempts) > 0 {
			au.Attempts[len(au.Attempts)-1].FallbackReason = "chain_advance:" + string(outcome)
		}
		log.Warn("streaming upstream error, advancing chain", "upstream", up.Cfg.Name, "status", resp.StatusCode)
	}
	if upstreamResp == nil {
		writeOpenAIError(w, 502, "upstream request failed")
		return
	}
	defer func() { _ = upstreamResp.Body.Close() }()

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
		created:      nowUnix(),
		au:           getAuditUpstream(r),
	}
	st.jsonEnc = json.NewEncoder(&st.jsonBuf)
	st.jsonEnc.SetEscapeHTML(false)

	// §B1 streaming partition: see a2o_stream for rationale, including
	// the 5%-of-total `other`-bucket tripwire that signals when the
	// deferred translate/write split needs to land.
	var timer *segmentTimer
	var reqID uint64
	if st.au != nil {
		timer = st.au.Timer
		reqID = st.au.ReqID
	}
	scanner := bufio.NewScanner(upstreamResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
	var eventType string

	for {
		scanStart := time.Now()
		if !scanner.Scan() {
			if timer != nil {
				timer.Add(segUpstreamProcessing, time.Since(scanStart))
			}
			break
		}
		if timer != nil {
			timer.Add(segUpstreamProcessing, time.Since(scanStart))
		}
		line := scanner.Text()
		// B4: bytes-from-upstream counter (see a2o_stream).
		Active.AddBytesDownstream(reqID, int64(len(line)+1))

		if after, ok0 := strings.CutPrefix(line, "event: "); ok0 {
			eventType = after
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

	scanErr := scanner.Err()
	if scanErr != nil {
		log.Warn("SSE scanner error", "error", scanErr)
	}

	// Finalize.
	st.finalize()

	// REVIEW B1/I1/I2: record stream completion outcome on the
	// upstream/key the chain settled on (may not be chain[0] when
	// fallback occurred). Mid-stream scanner errors classify as
	// network errors; clean termination resets consec failures
	// via AttemptOK.
	recordStreamAttempt(au, streamUpstream, streamKey, attemptStart, http.StatusOK, upstreamResp.Header, nil, scanErr)

	log.Info("Stream completed", "model", clientModel, "elapsed", time.Since(start))
}

// o2aStreamState tracks state during Anthropic->OpenAI SSE translation.
type o2aStreamState struct {
	clientModel  string
	w            http.ResponseWriter
	flusher      http.Flusher
	metrics      *state.Metrics
	includeUsage bool
	// au records the wall-clock of the first user-meaningful content
	// chunk for stream.first_token_ms per §4.3. Set on the first
	// tool_use / thinking / text/input_json/thinking delta — NOT on
	// the message_start "role: assistant" prelude, which is gateway
	// scaffolding and not a token. See markFirstContentDelta.
	au *AuditUpstream

	toolIndex       int
	inToolBlock     bool
	inThinkingBlock bool
	sentFirst       bool
	finishReason    string
	usage           *OpenAIUsage
	messageID       string
	created         int64
	jsonBuf         bytes.Buffer  // reused across emit calls to avoid per-chunk allocation
	jsonEnc         *json.Encoder // writes to jsonBuf, reuses internal encode state
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
			Delta: OpenAIChunkDelta{Role: "assistant", Content: new("")},
		})
		s.sentFirst = true

	case "content_block_start":
		if event.ContentBlock != nil {
			switch event.ContentBlock.Type {
			case "tool_use":
				s.inToolBlock = true
				markFirstContentDelta(s.au)
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
				})
			case "thinking":
				// OpenAI has no native thinking type. Wrap as <think>
				// tags in text content so it round-trips correctly.
				s.inThinkingBlock = true
				s.inToolBlock = false
				markFirstContentDelta(s.au)
				s.emitChunk(OpenAIChunkChoice{
					Index: 0,
					Delta: OpenAIChunkDelta{Content: strPtr("<think>")},
				})
			default:
				s.inToolBlock = false
			}
		}

	case "content_block_delta":
		if event.Delta == nil {
			return
		}
		switch event.Delta.Type {
		case "text_delta":
			markFirstContentDelta(s.au)
			s.emitChunk(OpenAIChunkChoice{
				Index: 0,
				Delta: OpenAIChunkDelta{Content: &event.Delta.Text},
			})

		case "input_json_delta":
			markFirstContentDelta(s.au)
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
			})

		case "thinking_delta":
			markFirstContentDelta(s.au)
			// Emit thinking content as text (inside <think> wrapper).
			s.emitChunk(OpenAIChunkChoice{
				Index: 0,
				Delta: OpenAIChunkDelta{Content: &event.Delta.Thinking},
			})
		}

	case "content_block_stop":
		if s.inToolBlock {
			s.toolIndex++
			s.inToolBlock = false
		}
		if s.inThinkingBlock {
			s.inThinkingBlock = false
			s.emitChunk(OpenAIChunkChoice{
				Index: 0,
				Delta: OpenAIChunkDelta{Content: strPtr("</think>\n\n")},
			})
		}

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
		if event.Error != nil && event.Error.Message != "" {
			errMsg = event.Error.Message
		}
		s.emitChunk(OpenAIChunkChoice{
			Index: 0,
			Delta: OpenAIChunkDelta{Content: &errMsg},
		})
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
	})

	// Emit usage chunk if requested (with empty choices per OpenAI spec).
	if s.includeUsage && s.usage != nil {
		s.emitUsageChunk(s.usage)
	}

	// Emit [DONE].
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

// emitUsageChunk emits a trailing usage-only chunk with choices: [] per OpenAI spec.
func (s *o2aStreamState) emitUsageChunk(usage *OpenAIUsage) {
	id := s.messageID
	if id == "" {
		id = "chatcmpl-stream"
	}
	chunk := ChatCompletionChunk{
		ID:      ensurePrefix(id, "chatcmpl-"),
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.clientModel,
		Choices: []OpenAIChunkChoice{},
		Usage:   usage,
	}
	s.jsonBuf.Reset()
	_ = s.jsonEnc.Encode(chunk)
	b := s.jsonBuf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	_, _ = io.WriteString(s.w, "data: ")
	_, _ = s.w.Write(b)
	_, _ = io.WriteString(s.w, "\n\n")
	s.flusher.Flush()
	s.metrics.BytesTx.Add(int64(len(b) + 8))
}

func (s *o2aStreamState) emitChunk(choice OpenAIChunkChoice) {
	id := s.messageID
	if id == "" {
		id = "chatcmpl-stream"
	}
	chunk := ChatCompletionChunk{
		ID:      ensurePrefix(id, "chatcmpl-"),
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.clientModel,
		Choices: []OpenAIChunkChoice{choice},
	}
	s.jsonBuf.Reset()
	_ = s.jsonEnc.Encode(chunk)
	b := s.jsonBuf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	_, _ = io.WriteString(s.w, "data: ")
	_, _ = s.w.Write(b)
	_, _ = io.WriteString(s.w, "\n\n")
	s.flusher.Flush()
	s.metrics.BytesTx.Add(int64(len(b) + 8))
}

//go:fix inline
func strPtr(s string) *string { return new(s) }
