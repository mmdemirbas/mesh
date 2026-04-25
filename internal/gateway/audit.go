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
	TopToolResult                *TopToolResultInfo // largest tool_result block in this request, if any
	RepeatReads                  *RepeatReadsInfo   // re-read activity this turn, if non-trivial
	StartTime                    time.Time
	EndTime                      time.Time
	Headers                      map[string][]string
	UpstreamReq                  []byte // translated request body sent upstream (set by handler)
	UpstreamResp                 []byte // raw upstream response body (non-streaming; set by handler)
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
	defer close(r.cleanupDone)
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-r.stopCleanup:
			return
		case <-t.C:
			r.cleanupOldFiles()
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
	return a.ResponseWriter.Write(p)
}

func (a *auditingWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recorderKey is a context key for carrying the recorder through to passthrough.
type recorderKey struct{}

// getRecorderFromContext retrieves the Recorder from the request context.
func getRecorderFromContext(r *http.Request) *Recorder {
	v, _ := r.Context().Value(recorderKey{}).(*Recorder)
	return v
}

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

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		upstream, r := WithAuditUpstream(r)

		// Walk the request body once for top_tool_result and
		// repeat_reads. analyzeRequest returns nil for branches that
		// would be omitted from the row anyway (no tool_results, or
		// trivial repeat-read activity). Stashing onto upstream keeps
		// the result in scope for the post-handler row-write below.
		topTR, repeatRR := analyzeRequest(body, clientAPI, sessionID, readIdx)
		upstream.TopToolResult = topTR
		upstream.RepeatReads = repeatRR

		aw := &auditingWriter{ResponseWriter: w}
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

		var summary *SSESummary
		var usage *Usage
		auditBody := decodeForAudit(aw.buf.Bytes(), aw.Header().Get("Content-Encoding"), nil)
		ct := aw.Header().Get("Content-Type")
		if strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
			summary = reassembleSSE(auditBody, clientAPI)
			if summary != nil {
				usage = summary.Usage
			}
		} else {
			usage = parseUsage(auditBody, clientAPI)
		}
		if usage != nil {
			metrics := state.Global.GetMetrics("gateway", gwName)
			metrics.TokensIn.Add(int64(usage.InputTokens))
			metrics.TokensOut.Add(int64(usage.OutputTokens))
			metrics.TokensCacheRd.Add(int64(usage.CacheReadInputTokens))
			metrics.TokensCacheWr.Add(int64(usage.CacheCreationInputTokens))
			metrics.TokensReason.Add(int64(usage.ReasoningTokens))
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
			StartTime:                    start,
			EndTime:                      time.Now(),
			Headers:                      aw.Header(),
			UpstreamReq:                  upstream.ReqBody,
			UpstreamResp:                 upstream.RespBody,
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
