package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
type RequestMeta struct {
	Gateway   string
	Direction string
	Model     string
	Stream    bool
	Method    string
	Path      string
	Headers   map[string][]string
	StartTime time.Time
}

// ResponseMeta is the structured context written to an audit response row.
type ResponseMeta struct {
	Status    int
	Outcome   string
	Usage     *Usage
	Summary   *SSESummary // optional reassembled SSE summary (streamed responses)
	StartTime time.Time
	EndTime   time.Time
	Headers   map[string][]string
}

// Usage is the token accounting captured per response. Either side may be zero
// when the upstream does not report that field (common for streamed OpenAI
// responses without stream_options.include_usage).
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
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
func NewRecorder(cfg GatewayCfg, log *slog.Logger) (*Recorder, error) {
	level := cfg.Log.ResolvedLevel()
	if level == LogLevelOff {
		return nil, nil
	}
	dir := expandHome(cfg.Log.ResolvedDir())
	gwDir := filepath.Join(dir, cfg.Name)
	if err := os.MkdirAll(gwDir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	r := &Recorder{
		gateway:     cfg.Name,
		dir:         gwDir,
		level:       level,
		maxSize:     cfg.Log.ResolvedMaxFileSize(),
		maxAge:      cfg.Log.ResolvedMaxAge(),
		log:         log.With("audit", cfg.Name),
		stopCleanup: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	r.cleanupOldFiles()
	go r.cleanupLoop()
	return r, nil
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
		"ts":        meta.StartTime.UTC().Format(time.RFC3339Nano),
		"gateway":   meta.Gateway,
		"direction": meta.Direction,
		"model":     meta.Model,
		"stream":    meta.Stream,
		"method":    meta.Method,
		"path":      meta.Path,
		"headers":   redactHeaders(meta.Headers),
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
	// Summary is cheap and highly useful — include it at metadata level too.
	if meta.Summary != nil {
		row["stream_summary"] = meta.Summary
	}
	if r.level == LogLevelFull {
		row["body"] = rawOrString(body)
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
