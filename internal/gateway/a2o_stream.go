package gateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

// generateMsgID returns a unique Anthropic-style message ID ("msg_" + 24 hex chars).
func generateMsgID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// handleA2OStream handles Direction A streaming: client expects Anthropic SSE,
// upstream returns OpenAI SSE. Reads OpenAI SSE chunks from upstream and
// translates each to Anthropic SSE events.
func handleA2OStream(w http.ResponseWriter, r *http.Request, oaiReq *ChatCompletionRequest, upstream *ResolvedUpstream, clientModel string, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	oaiBody, _ := json.Marshal(oaiReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = oaiBody
	}

	ctx := r.Context()
	if au := getAuditUpstream(r); au != nil {
		ctx = attachTimingTrace(ctx, au.Timer)
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, "POST", upstream.Cfg.Target, bytes.NewReader(oaiBody))
	if err != nil {
		writeAnthropicError(w, 500, "cannot create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if upstream.APIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	}

	upstreamResp, err := upstream.Client.Do(upstreamReq)
	if err != nil {
		writeAnthropicError(w, 502, "upstream request failed")
		log.Error("Upstream stream request failed", "error", err)
		return
	}
	defer func() { _ = upstreamResp.Body.Close() }()

	if upstreamResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(upstreamResp.Body, 4096))
		if au := getAuditUpstream(r); au != nil {
			au.RespBody = errBody
		}
		status := translateUpstreamErrorStatus(upstreamResp.StatusCode, DirA2O)
		writeAnthropicError(w, status, translatedUpstreamErrorMessage(errBody))
		log.Warn("Upstream stream error", "status", upstreamResp.StatusCode, "body", string(errBody))
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
		au:          getAuditUpstream(r),
	}
	st.jsonEnc = json.NewEncoder(&st.jsonBuf)
	st.jsonEnc.SetEscapeHTML(false)

	// Emit message_start.
	st.emitMessageStart()

	// §B1 streaming partition: time scanner.Scan() as
	// segUpstreamProcessing accumulator. Translate-only time inside
	// processChunk falls into `other` for v1 — splitting it from
	// embedded aw.Write calls requires deeper instrumentation that
	// is deferred. Write durations are tracked uniformly by
	// auditingWriter.Write.
	var timer *segmentTimer
	if st.au != nil {
		timer = st.au.Timer
	}
	scanner := bufio.NewScanner(upstreamResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
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

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			log.Warn("cannot parse SSE chunk", "error", err)
			continue
		}

		st.processChunk(&chunk)
	}

	if err := scanner.Err(); err != nil {
		log.Warn("SSE scanner error", "error", err)
	}

	// Finalize.
	st.finalize()

	log.Info("Stream completed", "model", clientModel, "elapsed", time.Since(start))
}

// a2oStreamState tracks state during OpenAI->Anthropic SSE translation.
type a2oStreamState struct {
	clientModel string
	w           http.ResponseWriter
	flusher     http.Flusher
	metrics     *state.Metrics
	// au is the per-request audit context. Used to record the
	// wall-clock of the first user-meaningful content delta
	// (text/thinking/input_json) for stream.first_token_ms per
	// §4.3. nil-safe for tests that bypass wrapAuditing.
	au *AuditUpstream

	blockIndex      int
	inTextBlock     bool
	inToolBlock     bool
	inThinkingBlock bool
	usage           AnthropicUsage
	stopReason      string
	hasBlock        bool
	jsonBuf         bytes.Buffer  // reused across emit calls to avoid per-chunk allocation
	jsonEnc         *json.Encoder // writes to jsonBuf, reuses internal encode state
	thinkFilter     thinkTagFilter
}

// markFirstContentDelta sets au.FirstContentDeltaAt to now if it
// hasn't been set yet. Safe under nil au and idempotent across the
// many emit-foo helpers that may all race to be "first".
func markFirstContentDelta(au *AuditUpstream) {
	if au == nil {
		return
	}
	if au.FirstContentDeltaAt.IsZero() {
		au.FirstContentDeltaAt = time.Now()
	}
}

// thinkTagFilter translates leading <think>...</think> XML wrappers in
// streamed text into native Anthropic thinking content. Some OpenAI-compatible
// upstreams embed model thinking as XML tags in text; Anthropic uses native
// thinking content blocks instead.
//
// States: 0=scanning (buffering to detect <think>), 1=inside <think> (emit
// as thinking), 2=done (passthrough as text).
type thinkTagFilter struct {
	state int
	buf   strings.Builder
}

func (f *thinkTagFilter) process(text string, emitText, emitThinking func(string)) {
	if f.state == 2 {
		emitText(text)
		return
	}
	f.buf.WriteString(text)
	f.drain(emitText, emitThinking)
}

func (f *thinkTagFilter) drain(emitText, emitThinking func(string)) {
	buf := f.buf.String()
	switch f.state {
	case 0: // scanning — haven't determined if there's a <think> yet
		trimmed := strings.TrimLeft(buf, " \t\n\r")
		if len(trimmed) == 0 {
			return
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "<think>") {
			// Found opening tag — extract content after it and switch to thinking state.
			idx := strings.Index(strings.ToLower(buf), "<think>")
			rest := buf[idx+7:]
			f.buf.Reset()
			f.buf.WriteString(rest)
			f.state = 1
			f.drain(emitText, emitThinking) // re-check for immediate </think>
			return
		}
		if len(trimmed) < 7 && strings.HasPrefix("<think>", lower) {
			return // partial match, keep buffering
		}
		// Not a <think> tag — flush buffer as text and passthrough.
		f.state = 2
		f.buf.Reset()
		emitText(buf)

	case 1: // inside <think> — emit as thinking until </think>
		closeIdx := strings.Index(strings.ToLower(buf), "</think>")
		if closeIdx < 0 {
			// Still inside — flush buffer as thinking content.
			f.buf.Reset()
			if len(buf) > 0 {
				emitThinking(buf)
			}
			return
		}
		// Found close tag. Emit thinking before it, text after it.
		before := buf[:closeIdx]
		after := strings.TrimLeft(buf[closeIdx+8:], "\n\r")
		f.state = 2
		f.buf.Reset()
		if len(before) > 0 {
			emitThinking(before)
		}
		if len(after) > 0 {
			emitText(after)
		}
	}
}

func (f *thinkTagFilter) flush(emitText func(string)) {
	if f.buf.Len() == 0 {
		return
	}
	buf := f.buf.String()
	f.buf.Reset()
	if f.state == 0 {
		// Never found <think>, emit buffered content as text.
		emitText(buf)
	}
	// state 1: stream ended inside <think> without </think> — already emitted
	// incrementally, nothing more to flush.
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

	// Text content delta. Filter through thinkTagFilter to translate
	// <think>...</think> wrappers into native Anthropic thinking blocks.
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		s.thinkFilter.process(*choice.Delta.Content,
			func(text string) {
				if !s.inTextBlock {
					s.startTextBlock()
				}
				s.emitTextDelta(text)
			},
			func(thinking string) {
				s.emitThinking(thinking)
			},
		)
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
			}

			// Argument delta.
			if tc.Function.Arguments != "" {
				s.emitInputJSONDelta(tc.Function.Arguments)
			}
		}
	}
}

// Typed structs for SSE events — avoids map[string]any allocations per chunk.

type a2oBlockStart struct {
	Type         string      `json:"type"`
	Index        int         `json:"index"`
	ContentBlock a2oBlockDef `json:"content_block"`
}

type a2oBlockDef struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

type a2oBlockDelta struct {
	Type  string        `json:"type"`
	Index int           `json:"index"`
	Delta a2oDeltaInner `json:"delta"`
}

type a2oDeltaInner struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type a2oBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type a2oMsgStart struct {
	Type    string    `json:"type"`
	Message a2oMsgDef `json:"message"`
}

type a2oMsgDef struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []any          `json:"content"`
	Usage   AnthropicUsage `json:"usage"`
}

type a2oMsgDelta struct {
	Type  string         `json:"type"`
	Delta a2oStopDelta   `json:"delta"`
	Usage AnthropicUsage `json:"usage"`
}

type a2oStopDelta struct {
	StopReason string `json:"stop_reason"`
}

type a2oMsgStop struct {
	Type string `json:"type"`
}

func (s *a2oStreamState) startTextBlock() {
	s.closeCurrentBlock()
	s.emit("content_block_start", a2oBlockStart{
		Type:         "content_block_start",
		Index:        s.blockIndex,
		ContentBlock: a2oBlockDef{Type: "text", Text: ""},
	})
	s.inTextBlock = true
	s.hasBlock = true
}

func (s *a2oStreamState) emitTextDelta(text string) {
	markFirstContentDelta(s.au)
	s.emit("content_block_delta", a2oBlockDelta{
		Type:  "content_block_delta",
		Index: s.blockIndex,
		Delta: a2oDeltaInner{Type: "text_delta", Text: text},
	})
}

func (s *a2oStreamState) emitThinking(thinking string) {
	markFirstContentDelta(s.au)
	if !s.inThinkingBlock {
		s.closeCurrentBlock()
		s.emit("content_block_start", a2oBlockStart{
			Type:         "content_block_start",
			Index:        s.blockIndex,
			ContentBlock: a2oBlockDef{Type: "thinking"},
		})
		s.inThinkingBlock = true
		s.hasBlock = true
	}
	s.emit("content_block_delta", a2oBlockDelta{
		Type:  "content_block_delta",
		Index: s.blockIndex,
		Delta: a2oDeltaInner{Type: "thinking_delta", Thinking: thinking},
	})
}

func (s *a2oStreamState) startToolBlock(id, name string) {
	s.emit("content_block_start", a2oBlockStart{
		Type:  "content_block_start",
		Index: s.blockIndex,
		ContentBlock: a2oBlockDef{
			Type:  "tool_use",
			ID:    id,
			Name:  name,
			Input: map[string]any{},
		},
	})
	s.inToolBlock = true
	s.hasBlock = true
}

func (s *a2oStreamState) emitInputJSONDelta(partial string) {
	markFirstContentDelta(s.au)
	s.emit("content_block_delta", a2oBlockDelta{
		Type:  "content_block_delta",
		Index: s.blockIndex,
		Delta: a2oDeltaInner{Type: "input_json_delta", PartialJSON: partial},
	})
}

func (s *a2oStreamState) closeCurrentBlock() {
	if s.inTextBlock || s.inToolBlock || s.inThinkingBlock {
		s.emit("content_block_stop", a2oBlockStop{
			Type:  "content_block_stop",
			Index: s.blockIndex,
		})
		s.blockIndex++
		s.inTextBlock = false
		s.inToolBlock = false
		s.inThinkingBlock = false
	}
}

func (s *a2oStreamState) emitMessageStart() {
	s.emit("message_start", a2oMsgStart{
		Type: "message_start",
		Message: a2oMsgDef{
			ID:      generateMsgID(),
			Type:    "message",
			Role:    "assistant",
			Model:   s.clientModel,
			Content: []any{},
		},
	})
}

func (s *a2oStreamState) finalize() {
	// Flush any remaining buffered text from think tag filter.
	s.thinkFilter.flush(func(text string) {
		if !s.inTextBlock {
			s.startTextBlock()
		}
		s.emitTextDelta(text)
	})

	s.closeCurrentBlock()

	if s.stopReason == "" {
		s.stopReason = "end_turn"
	}

	s.emit("message_delta", a2oMsgDelta{
		Type:  "message_delta",
		Delta: a2oStopDelta{StopReason: s.stopReason},
		Usage: s.usage,
	})

	s.emit("message_stop", a2oMsgStop{Type: "message_stop"})
}

func (s *a2oStreamState) emit(event string, data any) {
	s.jsonBuf.Reset()
	_ = s.jsonEnc.Encode(data)
	b := s.jsonBuf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1] // Encoder appends \n; trim for SSE format
	}
	_, _ = io.WriteString(s.w, "event: ")
	_, _ = io.WriteString(s.w, event)
	_, _ = io.WriteString(s.w, "\ndata: ")
	_, _ = s.w.Write(b)
	_, _ = io.WriteString(s.w, "\n\n")
	s.flusher.Flush()
	s.metrics.BytesTx.Add(int64(len(b) + len(event) + 20))
}
