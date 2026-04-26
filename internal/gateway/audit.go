package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmdemirbas/mesh/internal/nodeutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

// Audit outcome constants recorded in the `outcome` field of response rows.
const (
	OutcomeOK              = "ok"
	OutcomeError           = "error"
	OutcomeTruncated       = "truncated"
	OutcomeClientCancelled = "client_cancelled"
)

// maxAuditBodyBytes caps the body size copied into a single audit row. Keeps
// pathological requests/responses from blowing up the log. Matches the
// existing upstream response cap.
const maxAuditBodyBytes = 64 * 1024 * 1024

// redactedHeaders lists headers that must never appear verbatim in audit logs.
var redactedHeaders = map[string]struct{}{
	"authorization": {},
	"x-api-key":     {},
	"cookie":        {},
	"set-cookie":    {},
}

// RequestID correlates a request row with its response row. Monotonic within
// a process; not globally unique but sufficient for a local audit log.
type RequestID uint64

// RequestMeta is the structured context written to an audit request row.
//
// SessionID and TurnIndex are derived from the request body at capture time
// (see extractSessionInfo). They group turns of the same conversation in the
// audit UI without requiring any client cooperation: the first message of a
// chat is byte-stable across replays from history, so its hash is a sound
// conversation key.
type RequestMeta struct {
	Gateway     string
	Direction   string
	Model       string
	MappedModel string // upstream model after model_map (empty when no mapping)
	Stream      bool
	Method      string
	Path        string
	Headers     map[string][]string
	SessionID   string
	TurnIndex   int
	StartTime   time.Time
}

// ResponseMeta is the structured context written to an audit response row.
type ResponseMeta struct {
	Status                       int
	Outcome                      string
	Usage                        *Usage
	Summary                      *SSESummary // optional reassembled SSE summary (streamed responses)
	Summarized                   bool
	ContextWindowTokens          int
	OriginalInputTokensEstimate  int
	EffectiveInputTokensEstimate int
	TopToolResult                *TopToolResultInfo  // largest tool_result block in this request, if any
	RepeatReads                  *RepeatReadsInfo    // re-read activity this turn, if non-trivial
	ResponseBytes                *ResponseByteCounts // §4.3 partition of the decoded response body
	Stream                       *StreamInfo         // §4.3 stream accounting; emitted on every audited response
	Timing                       *TimingInfo         // §B1 six-segment timing partition; emitted on every audited response
	Summarize                    *SummarizeInfo      // §4.5 summarize delta; emitted only when fired
	StartTime                    time.Time
	EndTime                      time.Time
	Headers                      map[string][]string
	UpstreamReq                  []byte // translated request body sent upstream (set by handler)
	UpstreamResp                 []byte // raw upstream response body (non-streaming; set by handler)
	// Workstream A.6 — resilience-path forensics. Attempts is the
	// per-attempt history dispatchAcrossChain produced; FinalUpstream
	// and FinalKeyID name the (upstream, key) pair that served the
	// successful final response, or are empty when every attempt
	// failed. Empty across the board for legacy / non-chain configs
	// where dispatch made a single attempt — the row writer omits
	// Attempts when it has only one entry to keep audit rows tidy
	// for the dominant single-upstream case.
	Attempts       []Attempt
	FinalUpstream  string
	FinalKeyID     string
}

// TimingInfo is the §B1 six-segment timing partition. Always emitted
// on audited responses. The seven named segments plus Total partition
// the request's wall-clock with no overlap and no gap (Other absorbs
// residual). Total equals StreamInfo.TotalMs by construction —
// both are derived from the same (end - start) pair sampled in
// wrapAuditing.
//
// See DESIGN_B1_timing.local.md for the partition discipline, the
// per-segment capture points, and the sum-to-total invariant.
type TimingInfo struct {
	ClientToMesh       int64 `json:"client_to_mesh"`
	MeshTranslationIn  int64 `json:"mesh_translation_in"`
	MeshToUpstream     int64 `json:"mesh_to_upstream"`
	UpstreamProcessing int64 `json:"upstream_processing"`
	MeshTranslationOut int64 `json:"mesh_translation_out"`
	MeshToClient       int64 `json:"mesh_to_client"`
	Other              int64 `json:"other"`
	Total              int64 `json:"total"`
}

// buildTimingInfo converts a segmentTimer Snapshot plus the request's
// total wall-clock into the on-disk TimingInfo. Total is computed by
// the caller from the same start/end pair used for stream.total_ms,
// so timing_ms.total == stream.total_ms by construction (D1).
//
// Other is computed by subtraction (D2). Per-segment truncation to
// integer milliseconds happens here, exactly once per segment (D5).
// Snapshot durations are clamped to [0, total] before truncation so
// that an out-of-order callback that produced a near-total span
// cannot push other negative.
//
// Pure function so the partition math is unit-testable without the
// HTTP machinery.
func buildTimingInfo(snapshot map[timingSegment]time.Duration, totalMs int64) *TimingInfo {
	totalMs = max(totalMs, 0)
	total := time.Duration(totalMs) * time.Millisecond
	segMs := func(seg timingSegment) int64 {
		d := snapshot[seg]
		if d < 0 {
			return 0
		}
		if d > total {
			d = total
		}
		return d.Milliseconds()
	}
	t := &TimingInfo{
		ClientToMesh:       segMs(segClientToMesh),
		MeshTranslationIn:  segMs(segMeshTranslationIn),
		MeshToUpstream:     segMs(segMeshToUpstream),
		UpstreamProcessing: segMs(segUpstreamProcessing),
		MeshTranslationOut: segMs(segMeshTranslationOut),
		MeshToClient:       segMs(segMeshToClient),
		Total:              totalMs,
	}
	sum := t.ClientToMesh + t.MeshTranslationIn + t.MeshToUpstream +
		t.UpstreamProcessing + t.MeshTranslationOut + t.MeshToClient
	// Defensive: the per-segment clamp above bounds each value to
	// [0, total], so totalMs - sum is non-negative whenever total
	// equals end-start. The max() guard catches the case where a
	// future change breaks the clamp invariant.
	t.Other = max(totalMs-sum, 0)
	return t
}

// StreamInfo is the §4.3 stream-accounting block. Always emitted on
// audited responses (including non-streaming, where Terminated is
// "normal" and FirstTokenMs equals TotalMs).
type StreamInfo struct {
	// FirstTokenMs is wall-clock ms from request start to the first
	// content delta — text_delta / thinking_delta / input_json_delta
	// for Anthropic, delta.content / delta.reasoning_content /
	// delta.tool_calls.* for OpenAI. NOT first wire byte: Anthropic's
	// message_start prelude carries metadata, not user-meaningful
	// payload. Translation handlers (a2o_stream / o2a_stream) set
	// the timestamp on AuditUpstream when they emit the first such
	// delta; passthrough falls back to first non-zero Write because
	// it doesn't parse the upstream stream. For non-streaming
	// responses and degenerate streams (empty / cancelled before
	// any content), FirstTokenMs equals TotalMs by derivation in
	// the row writer — never null.
	FirstTokenMs int64 `json:"first_token_ms"`
	// TotalMs is wall-clock ms from request start to inner-handler
	// return (stream close for SSE, response write for JSON).
	TotalMs int64 `json:"total_ms"`
	// Terminated ∈ {"normal", "client", "upstream"}. See §4.3 for
	// the decision tree the reassembler + wrapAuditing apply.
	Terminated string `json:"terminated"`
}

// SummarizeInfo is the §4.5 summarize-delta block. Emitted only when
// summarization actually fired during the request (audit row's
// `summarize` key is omitted otherwise per §4.6 presence rule).
//
// Both byte counts measure the messages-array level — `pre` and
// `post` slices marshalled to JSON via encoding/json's default —
// not the full request body. The two numbers are messages-level
// byte counts; partition with §4 input_bytes is not implied.
type SummarizeInfo struct {
	Fired          bool `json:"fired"`
	TurnsCollapsed int  `json:"turns_collapsed,omitempty"`
	BytesRemoved   int  `json:"bytes_removed,omitempty"`
	BytesAdded     int  `json:"bytes_added,omitempty"`
}

// deriveFirstTokenMs implements §4.3 + reviewer's option C for
// first_token_ms. Resolution order:
//
//  1. Translation handler set FirstContentDeltaAt — use it.
//     Accurate "time to first content delta" semantic.
//  2. auditingWriter saw a non-zero Write — use that. For
//     non-streaming responses the whole body lands in one Write,
//     so this matches "time to response" exactly. For passthrough
//     streaming it's a heuristic (first byte is usually
//     message_start metadata, close to but not exactly the first
//     content delta).
//  3. Neither was set — degenerate cases (empty stream, client
//     cancellation before any byte). Fall back to TotalMs so the
//     invariant first_token_ms <= total_ms always holds and the
//     admin UI never sees null.
//
// Pure function so the resolution rules are unit-testable without
// reaching into wrapAuditing's hot path.
func deriveFirstTokenMs(start, end, contentDeltaAt, firstWriteAt time.Time) int64 {
	switch {
	case !contentDeltaAt.IsZero():
		return contentDeltaAt.Sub(start).Milliseconds()
	case !firstWriteAt.IsZero():
		return firstWriteAt.Sub(start).Milliseconds()
	default:
		return end.Sub(start).Milliseconds()
	}
}

// deriveStreamTerminated implements the §4.3 decision tree:
//
//   - reassembler said "normal" (terminal marker seen) → keep
//     "normal" regardless of client cancellation. The reviewer-
//     flagged race ("client closes after upstream sent
//     message_stop but before gateway flushes the closing frame")
//     is irrelevant: the upstream completed cleanly.
//   - reassembler said "upstream" AND request context cancelled
//     → "client". The leg was the client's choice, not an
//     upstream failure.
//   - everything else → pass through whatever the reassembler said.
//
// Pure function so the race semantics are unit-testable without
// contriving an HTTP client disconnect.
func deriveStreamTerminated(reassemblerSays string, ctxErr error) string {
	if reassemblerSays == "upstream" && ctxErr != nil {
		return "client"
	}
	return reassemblerSays
}

// Usage is the token accounting captured per response. Any field may be zero
// when the upstream does not report it (common for streamed OpenAI responses
// without stream_options.include_usage). Cache fields are Anthropic-specific
// and surface prompt-cache effectiveness; ReasoningTokens is OpenAI-specific
// (o-series). Kept in one struct so the audit row stays flat.
type Usage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
}

// isZero reports whether the usage struct carries any non-zero token count.
func (u *Usage) isZero() bool {
	if u == nil {
		return true
	}
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 &&
		u.ReasoningTokens == 0
}

// Recorder writes request/response audit rows to JSONL files. A nil receiver
// is a valid no-op recorder, so callers need not branch on whether logging
// was configured.
type Recorder struct {
	gateway string
	dir     string
	level   string
	maxSize int64
	maxAge  time.Duration
	log     *slog.Logger
	runID   string // short random id, unique per Recorder (per mesh process)

	nextID atomic.Uint64

	mu     sync.Mutex
	file   *os.File
	date   string
	size   int64
	closed bool

	stopCleanup chan struct{}
	cleanupDone chan struct{}
}

// NewRecorder constructs an audit recorder from a gateway's Log config. When
// the resolved level is "off" the function returns (nil, nil) so callers can
// assign the result directly — nil methods are safe.
func NewRecorder(gwName string, logCfg LogCfg, log *slog.Logger) (*Recorder, error) {
	level := logCfg.ResolvedLevel()
	if level == LogLevelOff {
		return nil, nil
	}
	dir := expandHome(logCfg.ResolvedDir())
	gwDir := filepath.Join(dir, gwName)
	if err := os.MkdirAll(gwDir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	r := &Recorder{
		gateway:     gwName,
		dir:         gwDir,
		level:       level,
		maxSize:     logCfg.ResolvedMaxFileSize(),
		maxAge:      logCfg.ResolvedMaxAge(),
		log:         log.With("audit", gwName),
		runID:       newRunID(),
		stopCleanup: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	r.cleanupOldFiles()
	go r.cleanupLoop()
	registerAuditDir(gwName, gwDir)
	return r, nil
}

// auditDirRegistry maps gateway name → audit directory for the lifetime of
// the mesh process. Populated by NewRecorder; the admin UI reads it to find
// the right directory even when the user overrode log.dir.
var (
	auditDirMu sync.RWMutex
	auditDirs  = map[string]string{}
)

func registerAuditDir(name, dir string) {
	auditDirMu.Lock()
	auditDirs[name] = dir
	auditDirMu.Unlock()
}

func unregisterAuditDir(name string) {
	auditDirMu.Lock()
	delete(auditDirs, name)
	auditDirMu.Unlock()
}

// AuditDirs returns a snapshot of the gateway-name → audit-dir registry.
// Empty when no gateway has audit logging enabled.
func AuditDirs() map[string]string {
	auditDirMu.RLock()
	defer auditDirMu.RUnlock()
	out := make(map[string]string, len(auditDirs))
	maps.Copy(out, auditDirs)
	return out
}

// Request writes a "req" row and returns the correlation ID. A zero body is
// recorded at "metadata" level; at "full" level the body is embedded as raw
// JSON when it parses, otherwise as a string.
func (r *Recorder) Request(meta RequestMeta, body []byte) RequestID {
	if r == nil {
		return 0
	}
	id := RequestID(r.nextID.Add(1))
	row := map[string]any{
		"t":         "req",
		"id":        uint64(id),
		"run":       r.runID,
		"ts":        meta.StartTime.UTC().Format(time.RFC3339Nano),
		"gateway":   meta.Gateway,
		"direction": meta.Direction,
		"model":     meta.Model,
		"stream":    meta.Stream,
		"method":    meta.Method,
		"path":      meta.Path,
		"headers":   redactHeaders(meta.Headers),
	}
	if meta.MappedModel != "" && meta.MappedModel != meta.Model {
		row["mapped_model"] = meta.MappedModel
	}
	if meta.SessionID != "" {
		row["session_id"] = meta.SessionID
	}
	if meta.TurnIndex > 0 {
		row["turn_index"] = meta.TurnIndex
	}
	if r.level == LogLevelFull {
		row["body"] = rawOrString(body)
	}
	r.writeRow(row)
	return id
}

// Response writes a "resp" row correlated with the given id.
func (r *Recorder) Response(id RequestID, meta ResponseMeta, body []byte) {
	if r == nil {
		return
	}
	elapsed := meta.EndTime.Sub(meta.StartTime)
	if meta.EndTime.IsZero() {
		elapsed = 0
	}
	row := map[string]any{
		"t":          "resp",
		"id":         uint64(id),
		"run":        r.runID,
		"ts":         meta.EndTime.UTC().Format(time.RFC3339Nano),
		"gateway":    r.gateway,
		"status":     meta.Status,
		"outcome":    meta.Outcome,
		"elapsed_ms": elapsed.Milliseconds(),
		"headers":    redactHeaders(meta.Headers),
	}
	if meta.Usage != nil {
		row["usage"] = meta.Usage
	}
	if meta.Summarized {
		row["summarized"] = true
	}
	if meta.ContextWindowTokens > 0 {
		row["context_window_tokens"] = meta.ContextWindowTokens
	}
	if meta.OriginalInputTokensEstimate > 0 {
		row["original_input_tokens_estimate"] = meta.OriginalInputTokensEstimate
	}
	if meta.EffectiveInputTokensEstimate > 0 {
		row["effective_input_tokens_estimate"] = meta.EffectiveInputTokensEstimate
	}
	if meta.TopToolResult != nil {
		row["top_tool_result"] = meta.TopToolResult
	}
	if meta.RepeatReads != nil {
		row["repeat_reads"] = meta.RepeatReads
	}
	if meta.ResponseBytes != nil {
		row["response_bytes"] = meta.ResponseBytes
	}
	if meta.Stream != nil {
		row["stream"] = meta.Stream
	}
	if meta.Timing != nil {
		row["timing_ms"] = meta.Timing
	}
	if meta.Summarize != nil {
		row["summarize"] = meta.Summarize
	}
	// Summary is cheap and highly useful — include it at metadata level too.
	if meta.Summary != nil {
		row["stream_summary"] = meta.Summary
	}
	if r.level == LogLevelFull {
		row["body"] = rawOrString(body)
		if len(meta.UpstreamReq) > 0 {
			row["upstream_req"] = rawOrString(meta.UpstreamReq)
		}
		if len(meta.UpstreamResp) > 0 {
			row["upstream_resp"] = rawOrString(meta.UpstreamResp)
		}
	}
	// A.6 resilience-path fields. The `attempts` array is gated on
	// >1 attempt — single-attempt rows would just repeat the upstream
	// info. But `final_upstream` / `final_key_id` are always emitted
	// when non-empty, so the operator can correlate every audit row
	// with the (upstream, key) pair that served it without having to
	// reconstruct it from headers. REVIEW #5: previously these were
	// hidden behind the >1 gate so single-attempt rows had no key
	// attribution at all.
	if len(meta.Attempts) > 1 {
		row["attempts"] = meta.Attempts
	}
	if meta.FinalUpstream != "" {
		row["final_upstream"] = meta.FinalUpstream
	}
	if meta.FinalKeyID != "" {
		row["final_key_id"] = meta.FinalKeyID
	}
	r.writeRow(row)
}

// Close flushes and closes the recorder. Safe to call multiple times.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.stopCleanup)
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	r.mu.Unlock()
	<-r.cleanupDone
	unregisterAuditDir(r.gateway)
	return err
}

func (r *Recorder) writeRow(row map[string]any) {
	data, err := json.Marshal(row)
	if err != nil {
		r.log.Warn("audit marshal failed", "error", err)
		return
	}
	data = append(data, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if err := r.ensureFileLocked(int64(len(data))); err != nil {
		r.log.Warn("audit open failed", "error", err)
		return
	}
	n, err := r.file.Write(data)
	if err != nil {
		r.log.Warn("audit write failed", "error", err)
		return
	}
	r.size += int64(n)
}

// ensureFileLocked opens the current day's file, rolling over by date or size
// as needed. Callers must hold r.mu.
func (r *Recorder) ensureFileLocked(incoming int64) error {
	today := time.Now().UTC().Format("2006-01-02")
	if r.file != nil && r.date == today && r.size+incoming <= r.maxSize {
		return nil
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	path := r.nextPathLocked(today)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.file = f
	r.date = today
	r.size = info.Size()
	return nil
}

// nextPathLocked picks the next path for the given date, probing for the
// first unused -N suffix when the base file is already full. Callers must
// hold r.mu.
func (r *Recorder) nextPathLocked(date string) string {
	base := filepath.Join(r.dir, date+".jsonl")
	if info, err := os.Stat(base); err != nil || info.Size()+1 <= r.maxSize {
		return base
	}
	for n := 1; n < 1_000_000; n++ {
		p := filepath.Join(r.dir, fmt.Sprintf("%s-%d.jsonl", date, n))
		if info, err := os.Stat(p); err != nil || info.Size()+1 <= r.maxSize {
			return p
		}
	}
	return base
}

func (r *Recorder) cleanupLoop() {
	// `defer close(r.cleanupDone)` MUST stay at the top so it fires
	// on every exit path including a deferred-recover return — Close
	// blocks on this channel and a never-closed channel turns a
	// graceful shutdown into a hang requiring SIGKILL.
	defer close(r.cleanupDone)
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-r.stopCleanup:
			return
		case <-t.C:
			// Per-tick recovery: a panic inside cleanupOldFiles
			// (rare filesystem race, NFS quirk, future code change)
			// must not end the loop AND must not break the
			// cleanupDone close above. The closure scopes the
			// recovery to one tick.
			func() {
				defer nodeutil.RecoverPanic("gateway.audit.cleanupOldFiles")
				r.cleanupOldFiles()
			}()
		}
	}
}

func (r *Recorder) cleanupOldFiles() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-r.maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(r.dir, e.Name()))
		}
	}
}

// redactHeaders returns a copy of h with sensitive values replaced. Empty or
// nil input returns nil.
func redactHeaders(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if _, bad := redactedHeaders[strings.ToLower(k)]; bad {
			out[k] = []string{"[redacted]"}
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}

// rawOrString renders the body as raw JSON when it parses, otherwise as a
// plain string. This keeps structured payloads queryable in jq without
// double-encoding, while still capturing non-JSON content (SSE text, error
// text) losslessly.
func rawOrString(body []byte) any {
	if len(body) == 0 {
		return ""
	}
	trimmed := body
	if len(trimmed) > maxAuditBodyBytes {
		trimmed = trimmed[:maxAuditBodyBytes]
	}
	if json.Valid(trimmed) {
		return json.RawMessage(trimmed)
	}
	return string(trimmed)
}

// AuditUpstream carries the translated upstream request body and (for
// non-streaming) the raw upstream response body back from the handler to
// wrapAuditing. The handler populates the fields; the wrapper reads them
// after the handler returns.
type AuditUpstream struct {
	ReqBody                      []byte // translated request sent to upstream
	RespBody                     []byte // raw upstream response (non-streaming only)
	Summarized                   bool
	ContextWindowTokens          int
	OriginalInputTokensEstimate  int
	EffectiveInputTokensEstimate int
	TopToolResult                *TopToolResultInfo
	RepeatReads                  *RepeatReadsInfo
	// FirstContentDeltaAt is set by translation streaming handlers
	// (a2o_stream / o2a_stream) when the first user-meaningful
	// content delta is emitted to the client. Zero value means "not
	// observed" — wrapAuditing falls back to the auditing writer's
	// firstWriteAt heuristic, which is correct for passthrough
	// (where the gateway doesn't parse the upstream stream) and
	// approximately correct for translation paths if the translator
	// didn't set it.
	FirstContentDeltaAt time.Time
	// SummarizeBytesRemoved / Added / TurnsCollapsed populated by
	// checkAndSummarize when summarization fires; emitted via
	// SummarizeInfo per §4.5.
	SummarizeBytesRemoved   int
	SummarizeBytesAdded     int
	SummarizeTurnsCollapsed int
	// Timer is the per-request segmentTimer for the §B1 timing
	// partition. wrapAuditing creates it at request start and
	// stashes it here so handlers, httptrace callbacks, and
	// streaming loops can call Mark / Pause / Add. nil-safe; tests
	// that bypass wrapAuditing do not initialize it.
	Timer *segmentTimer
	// ReqID lets handlers and the httptrace callbacks update the
	// B4 active registry alongside the timer. Zero when wrapAuditing
	// did not run (no recorder), in which case Active.* calls are
	// no-ops.
	ReqID uint64
	// SessionID is the request's session id (captured by
	// extractSessionInfo) plumbed through here so the dispatch
	// wrapper can consult it for sticky_session rotation. Empty
	// when no session header was set and no fingerprint fallback
	// applied.
	SessionID string
	// Attempts is the per-attempt history A.4's dispatch wrapper
	// records. Surfaced into the audit row by A.6 (the row writer
	// reads this field after the handler returns).
	Attempts []Attempt
	// Chain is the routing rule's resolved chain of upstreams
	// (A.5). The handler walks it left-to-right via
	// dispatchAcrossChain when it has multiple elements; single-
	// element chains short-circuit to the legacy dispatch.
	Chain []*ResolvedUpstream
}

type auditUpstreamKey struct{}

// WithAuditUpstream attaches an AuditUpstream to the request context.
func WithAuditUpstream(r *http.Request) (*AuditUpstream, *http.Request) {
	u := &AuditUpstream{}
	return u, r.WithContext(context.WithValue(r.Context(), auditUpstreamKey{}, u))
}

// getAuditUpstream retrieves the AuditUpstream from the request context, or nil.
func getAuditUpstream(r *http.Request) *AuditUpstream {
	v, _ := r.Context().Value(auditUpstreamKey{}).(*AuditUpstream)
	return v
}

// auditingWriter wraps an http.ResponseWriter to capture a capped copy of the
// bytes written and the final status code, while preserving streaming Flush
// behavior. Used by wrapAuditing to tee translation-handler output into the
// audit log without modifying each handler.
type auditingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	capped bool
	// firstWriteAt is the wall-clock of the first non-zero Write to
	// the wrapper. Used as a fallback for stream.first_token_ms when
	// the inner handler did not set FirstContentDeltaAt — accurate
	// for non-streaming responses (where the whole body lands in one
	// Write) and a heuristic for passthrough streaming (where the
	// first byte is usually the upstream's tiny message_start
	// metadata, close to "first token" within tens of milliseconds).
	firstWriteAt time.Time
	// timer is the per-request §B1 segmentTimer. On the first
	// non-empty Write to a non-streaming response the wrapper Marks
	// segMeshToClient. For streaming responses (Content-Type
	// text/event-stream) the Mark is suppressed — the streaming
	// handler manages its own segment 5/6 split via Pause + Add.
	timer *segmentTimer
	// reqID lets the Write hook bump the B4 active-registry's
	// per-request bytes_to_client counter and Mark its current
	// segment without a separate plumbing layer. Zero when the
	// recorder is nil (no audit), in which case registry calls are
	// no-ops anyway.
	reqID uint64
}

func (a *auditingWriter) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditingWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	pre := time.Now()
	if a.firstWriteAt.IsZero() && len(p) > 0 {
		a.firstWriteAt = pre
		// §B1 unified segment-6 entry: on the first non-empty
		// Write, close whatever segment was open (typically
		// segMeshTranslationOut, set by httptrace's
		// GotFirstResponseByte) so the underlying ResponseWriter.Write
		// duration is the only thing attributed to mesh_to_client.
		// For streaming responses the handler may have already
		// Paused the timer before reaching this point; Pause on a
		// paused timer is a no-op.
		if a.timer != nil {
			a.timer.Pause(pre)
		}
		// B4: reflect the segment-6 transition in the active
		// registry so the operator's chrome indicator and per-request
		// view see the phase advance live.
		Active.UpdatePhase(a.reqID, string(segMeshToClient), pre)
	}
	if !a.capped {
		remain := int64(maxAuditBodyBytes) - int64(a.buf.Len())
		if remain > 0 {
			take := int64(len(p))
			if take > remain {
				take = remain
				a.capped = true
			}
			a.buf.Write(p[:take])
		} else {
			a.capped = true
		}
	}
	n, err := a.ResponseWriter.Write(p)
	if a.timer != nil {
		a.timer.Add(segMeshToClient, time.Since(pre))
	}
	// B4: bytes-to-client counter — atomic, hot-path safe.
	if n > 0 {
		Active.AddBytesToClient(a.reqID, int64(n))
	}
	return n, err
}

func (a *auditingWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recorderKey is a context key for carrying the recorder through to passthrough.
type recorderKey struct{}

// wrapAuditing emits request/response audit rows around an existing handler.
// It peeks model/stream from the client request body, replays the body into
// r.Body so the inner handler can parse it, tees the client-facing response
// bytes, and reassembles SSE when the handler emits text/event-stream.
// clientAPI controls which SSE grammar to reassemble against (the handler
// emits in the client's format).
//
// When recorder is nil the wrapper is a no-op — callers never need to branch.
//
// readIdx is the per-gateway readIndex used to compute repeat_reads;
// pass nil to skip repeat-read tracking (e.g. unit tests of the audit
// path itself).
func wrapAuditing(gwName string, upstreamCfg *UpstreamCfg, clientAPI string, recorder *Recorder, readIdx *readIndex, inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Stash recorder in context so passthrough handler can access it.
		r = r.WithContext(context.WithValue(r.Context(), recorderKey{}, recorder))

		if recorder == nil {
			inner(w, r)
			return
		}
		start := time.Now()
		timer := newSegmentTimer()
		timer.Mark(segClientToMesh, start)

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
		if err != nil {
			http.Error(w, "request body read failed", http.StatusBadRequest)
			return
		}
		if int64(len(body)) > maxRequestBodySize {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		var peek struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &peek)
		sessionID, turnIndex := extractSessionInfo(r.Header, body)

		dir := ResolveDirection(clientAPI, upstreamCfg.API)
		mapped := upstreamCfg.MapModel(peek.Model)
		reqID := recorder.Request(RequestMeta{
			Gateway:     gwName,
			Direction:   dir.String(),
			Model:       peek.Model,
			MappedModel: mapped,
			Stream:      peek.Stream,
			Method:      r.Method,
			Path:        r.URL.Path,
			Headers:     r.Header,
			SessionID:   sessionID,
			TurnIndex:   turnIndex,
			StartTime:   start,
		}, body)

		// B4 active-registry lifecycle: register on entry, unregister
		// in defer so panics can't leak entries. The defer runs
		// after the audit row writes below, so the snapshot lifetime
		// is "in-flight" from any operator's perspective.
		Active.Register(&ActiveRequest{
			ID:          uint64(reqID),
			Gateway:     gwName,
			SessionID:   sessionID,
			ClientModel: peek.Model,
			Streaming:   peek.Stream,
			StartedAt:   start,
		})
		// upstreamModel is set under phaseMu via SetUpstreamModel;
		// see active_registry.go's note on the field's lock coverage.
		Active.SetUpstreamModel(uint64(reqID), mapped)
		defer Active.Unregister(uint64(reqID))
		// Bytes upstream is the request body size — fully buffered
		// before the inner handler dispatches it.
		Active.AddBytesUpstream(uint64(reqID), int64(len(body)))
		Active.UpdatePhase(uint64(reqID), string(segClientToMesh), start)

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		upstream, r := WithAuditUpstream(r)
		upstream.Timer = timer
		upstream.ReqID = uint64(reqID)
		upstream.SessionID = sessionID

		// Walk the request body once for top_tool_result and
		// repeat_reads. analyzeRequest returns nil for branches that
		// would be omitted from the row anyway (no tool_results, or
		// trivial repeat-read activity). Stashing onto upstream keeps
		// the result in scope for the post-handler row-write below.
		topTR, repeatRR := analyzeRequest(body, clientAPI, sessionID, readIdx)
		upstream.TopToolResult = topTR
		upstream.RepeatReads = repeatRR

		aw := &auditingWriter{ResponseWriter: w, timer: timer, reqID: uint64(reqID)}
		// Close segment 1 (client_to_mesh) and open segment 2
		// (mesh_translation_in) at the inner-handler entry boundary.
		// Segments 3-5 are Marked from httptrace.ClientTrace
		// callbacks attached by the upstream-dispatch handlers.
		// Segment 6 opens at firstWriteAt for non-streaming; the
		// streaming case manages its own boundary in subsequent
		// commits.
		now := time.Now()
		timer.Mark(segMeshTranslationIn, now)
		Active.UpdatePhase(uint64(reqID), string(segMeshTranslationIn), now)
		inner(aw, r)

		status := aw.status
		if status == 0 {
			status = http.StatusOK
		}
		outcome := OutcomeOK
		if status >= 400 {
			outcome = OutcomeError
		}
		if r.Context().Err() != nil {
			outcome = OutcomeClientCancelled
		} else if aw.capped && outcome == OutcomeOK {
			outcome = OutcomeTruncated
		}

		end := time.Now()
		var summary *SSESummary
		var usage *Usage
		var responseBytes *ResponseByteCounts
		totalMs := end.Sub(start).Milliseconds()
		streamInfo := &StreamInfo{
			Terminated:   "normal",
			TotalMs:      totalMs,
			FirstTokenMs: deriveFirstTokenMs(start, end, upstream.FirstContentDeltaAt, aw.firstWriteAt),
		}
		// §B1 timing partition — same end / start pair as
		// streamInfo.TotalMs so the two field values are identical
		// by construction (D1).
		timing := buildTimingInfo(timer.Snapshot(end), totalMs)
		auditBody := decodeForAudit(aw.buf.Bytes(), aw.Header().Get("Content-Encoding"), nil)
		ct := aw.Header().Get("Content-Type")
		isStreaming := strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
		if isStreaming {
			summary = reassembleSSE(auditBody, clientAPI)
			if summary != nil {
				usage = summary.Usage
				responseBytes = summary.ResponseBytes
				streamInfo.Terminated = deriveStreamTerminated(summary.Terminated, r.Context().Err())
			}
		} else {
			usage = parseUsage(auditBody, clientAPI)
			responseBytes = parseResponseBytes(auditBody, clientAPI)
			// Non-streaming has no mid-stream termination concept —
			// either the response landed (normal) or the upstream
			// erred earlier and we never reached this point. The
			// audit row's status field disambiguates 4xx/5xx vs 2xx.
		}
		if usage != nil {
			metrics := state.Global.GetMetrics("gateway", gwName)
			metrics.TokensIn.Add(int64(usage.InputTokens))
			metrics.TokensOut.Add(int64(usage.OutputTokens))
			metrics.TokensCacheRd.Add(int64(usage.CacheReadInputTokens))
			metrics.TokensCacheWr.Add(int64(usage.CacheCreationInputTokens))
			metrics.TokensReason.Add(int64(usage.ReasoningTokens))
		}

		var summarizeInfo *SummarizeInfo
		if upstream.Summarized {
			summarizeInfo = &SummarizeInfo{
				Fired:          true,
				TurnsCollapsed: upstream.SummarizeTurnsCollapsed,
				BytesRemoved:   upstream.SummarizeBytesRemoved,
				BytesAdded:     upstream.SummarizeBytesAdded,
			}
		}
		recorder.Response(reqID, ResponseMeta{
			Status:                       status,
			Outcome:                      outcome,
			Usage:                        usage,
			Summary:                      summary,
			Summarized:                   upstream.Summarized,
			ContextWindowTokens:          upstream.ContextWindowTokens,
			OriginalInputTokensEstimate:  upstream.OriginalInputTokensEstimate,
			EffectiveInputTokensEstimate: upstream.EffectiveInputTokensEstimate,
			TopToolResult:                upstream.TopToolResult,
			RepeatReads:                  upstream.RepeatReads,
			ResponseBytes:                responseBytes,
			Stream:                       streamInfo,
			Timing:                       timing,
			Summarize:                    summarizeInfo,
			StartTime:                    start,
			EndTime:                      end,
			Headers:                      aw.Header(),
			UpstreamReq:                  upstream.ReqBody,
			UpstreamResp:                 upstream.RespBody,
			Attempts:                     upstream.Attempts,
			FinalUpstream:                deriveFinalUpstream(upstream.Attempts),
			FinalKeyID:                   deriveFinalKeyID(upstream.Attempts),
		}, auditBody)
	}
}

// newRunID returns a short hex token used to disambiguate audit ids across
// mesh process restarts. nextID resets per-process; runID does not collide.
func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// expandHome resolves a leading "~/" to the user's home directory. Any other
// form is returned as-is.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
