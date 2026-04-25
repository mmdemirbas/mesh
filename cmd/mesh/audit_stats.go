package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
)

// auditStatsResponse is the shape returned by GET /api/gateway/audit/stats.
// Tokens are split so the UI can render the cache-vs-fresh stack without any
// further math; cache hit ratio is precomputed because clients otherwise have
// to handle the divide-by-zero case in three places.
type auditStatsResponse struct {
	Window         string                 `json:"window"`
	Bucket         string                 `json:"bucket"`
	Totals         auditStatsTotals       `json:"totals"`
	ByModel        []auditStatsRow        `json:"by_model"`
	BySession      []auditStatsRow        `json:"by_session"`
	ByPath         []auditStatsRow        `json:"by_path"`
	ByHour         []auditStatsHourRow    `json:"by_hour"`
	TopRequests    []auditStatsTopRequest `json:"top_requests"`
	PreambleBlocks []auditStatsRow        `json:"preamble_blocks"`
	Series         []auditStatsBucket     `json:"series"`
}

// auditStatsHourRow is a sparse hour-of-day bucket (0..23). Used so the
// overview can show whether token usage clumps at certain hours.
type auditStatsHourRow struct {
	Hour         int   `json:"hour"`
	Requests     int   `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// auditStatsTopRequest points back at one specific pair so the UI can
// open its detail card.
type auditStatsTopRequest struct {
	ID           uint64 `json:"id"`
	Run          string `json:"run"`
	TS           string `json:"ts"`
	Model        string `json:"model"`
	Session      string `json:"session"`
	Path         string `json:"path"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

type auditStatsTotals struct {
	Requests          int     `json:"requests"`
	Errors            int     `json:"errors"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_creation_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	ElapsedSumMs      int64   `json:"elapsed_sum_ms"`
	// ContentBreakdown aggregates per-section input bytes across the
	// window per SPEC §4.1. Computed on read by re-parsing each row's
	// body via gateway.SectionBytes — see SPEC §4.6 persist-vs-compute
	// pin. Only populated when the audit log is at "full" level
	// (bodies present); zero values otherwise.
	ContentBreakdown auditStatsContentBreakdown `json:"content_breakdown"`
}

// auditStatsContentBreakdown is the aggregated SectionByteCounts
// across every request in the query window. The fields mirror
// gateway.SectionByteCounts but use int64 for accumulation. Total
// equals the sum of all named fields, matching the §4.1 partition
// invariant in aggregate.
type auditStatsContentBreakdown struct {
	System      int64 `json:"system"`
	Tools       int64 `json:"tools"`
	UserText    int64 `json:"user_text"`
	Preamble    int64 `json:"preamble"`
	ToolResults int64 `json:"tool_results"`
	Thinking    int64 `json:"thinking"`
	ImagesWire  int64 `json:"images_wire"`
	UserHistory int64 `json:"user_history"`
	Other       int64 `json:"other"`
	Total       int64 `json:"total"`
	// Rows is the number of req rows that contributed to the
	// breakdown — i.e., rows where the body was present and parsed
	// into a non-zero partition. When this is 0 (metadata-level log,
	// or no requests in window) the UI surfaces a "enable full
	// logging for content breakdown" hint.
	Rows int `json:"rows"`
}

// auditStatsRow groups counters by a single key (model name, session id, …).
// FirstModel is populated only for session rows so the UI can show "session
// foo (opus)" without a second join.
type auditStatsRow struct {
	Key               string `json:"key"`
	Requests          int    `json:"requests"`
	Turns             int    `json:"turns,omitempty"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheCreateTokens int64  `json:"cache_creation_tokens"`
	FirstSeen         string `json:"first_seen,omitempty"`
	LastSeen          string `json:"last_seen,omitempty"`
	FirstModel        string `json:"first_model,omitempty"`
	// Paths lists project paths touched by this session (session rows only),
	// comma-joined. Sessions typically span one project; when a session talks
	// to multiple, all of them show up so the UI never silently hides a path.
	Paths string `json:"paths,omitempty"`
}

// auditStatsBucket is one point of the time series. Bucket boundaries are
// truncated so the same call is cacheable across requests.
type auditStatsBucket struct {
	T                 string `json:"t"`
	Requests          int    `json:"requests"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheCreateTokens int64  `json:"cache_creation_tokens"`
}

type statsFilter struct {
	session string
	model   string
	since   time.Time
	until   time.Time
	bucket  time.Duration
}

// computeAuditStats walks every audit file in dir, aggregates pairs that match
// the filter, and returns totals + breakdowns. Pair attribution: each
// req/resp pair is counted once; tokens come from the response row, while
// model/session come from the request row.
//
// Memory is bound by the open-pair map size — pathological logs with many
// orphaned requests are still capped because the parser ignores rows it
// cannot match within the same file walk window.
func computeAuditStats(dir string, f statsFilter) (auditStatsResponse, error) {
	files, err := listJSONLByMTimeDesc(dir)
	if err != nil {
		return auditStatsResponse{}, err
	}

	pairs := map[pairKey]*statsPair{}

	for _, e := range files {
		path := filepath.Join(dir, e.Name())
		ferr := scanFile(path, func(line []byte) bool {
			row, ok := parseStatsRow(line)
			if !ok {
				return true
			}
			if !f.since.IsZero() && row.ts.Before(f.since) {
				return true
			}
			if !f.until.IsZero() && row.ts.After(f.until) {
				return true
			}
			key := pairKey{id: row.id, run: row.run}
			pa, exists := pairs[key]
			if !exists {
				pa = &statsPair{}
				pairs[key] = pa
			}
			switch row.t {
			case "req":
				pa.req = row
				pa.haveReq = true
			case "resp":
				pa.resp = row
				pa.haveResp = true
			}
			return true
		})
		if ferr != nil && !errors.Is(ferr, errStopScan) {
			return auditStatsResponse{}, ferr
		}
	}

	var resp auditStatsResponse
	resp.Window = formatWindow(f.since, f.until)
	resp.Bucket = f.bucket.String()

	models := map[string]*auditStatsRow{}
	sessions := map[string]*auditStatsRow{}
	paths := map[string]*auditStatsRow{}
	preambles := map[string]*auditStatsRow{}
	hours := map[int]*auditStatsHourRow{}
	series := map[time.Time]*auditStatsBucket{}
	var topCandidates []auditStatsTopRequest

	for _, pa := range pairs {
		if !pa.haveReq {
			continue
		}
		if f.model != "" && pa.req.model != f.model {
			continue
		}
		if f.session != "" && pa.req.sessionID != f.session {
			continue
		}

		resp.Totals.Requests++
		if pa.haveResp {
			if pa.resp.outcome != "" && pa.resp.outcome != "ok" {
				resp.Totals.Errors++
			}
			resp.Totals.InputTokens += int64(pa.resp.inputTokens)
			resp.Totals.OutputTokens += int64(pa.resp.outputTokens)
			resp.Totals.CacheReadTokens += int64(pa.resp.cacheReadTokens)
			resp.Totals.CacheCreateTokens += int64(pa.resp.cacheCreateTokens)
			resp.Totals.ReasoningTokens += int64(pa.resp.reasoningTokens)
			resp.Totals.ElapsedSumMs += pa.resp.elapsedMs
		}

		mrow := upsertRow(models, pa.req.model)
		applyPair(mrow, pa)

		// Prefer a human-readable project name extracted from the system prompt
		// over the raw URL path. Falls back to the URL path when the body has
		// no working-directory hint (non-Claude-Code clients, passthrough mode).
		projectKey := extractProjectPath(pa.req.body)
		if projectKey == "" {
			projectKey = pa.req.path
		}

		if pa.req.sessionID != "" {
			srow := upsertRow(sessions, pa.req.sessionID)
			applyPair(srow, pa)
			if srow.FirstModel == "" {
				srow.FirstModel = pa.req.model
			}
			if pa.req.turnIndex > srow.Turns {
				srow.Turns = pa.req.turnIndex
			}
			if projectKey != "" && !containsCSV(srow.Paths, projectKey) {
				if srow.Paths == "" {
					srow.Paths = projectKey
				} else {
					srow.Paths += ", " + projectKey
				}
			}
		}

		if projectKey != "" {
			applyPair(upsertRow(paths, projectKey), pa)
		}

		if !pa.req.ts.IsZero() {
			h := pa.req.ts.UTC().Hour()
			hr, ok := hours[h]
			if !ok {
				hr = &auditStatsHourRow{Hour: h}
				hours[h] = hr
			}
			hr.Requests++
			if pa.haveResp {
				hr.InputTokens += int64(pa.resp.inputTokens)
				hr.OutputTokens += int64(pa.resp.outputTokens)
			}
		}

		// Preamble signature = tag name + first 40 chars of the body,
		// collapsed whitespace. Same-content injected blocks collapse
		// into one bucket so the UI reveals the biggest wasteful
		// recurring block (e.g. "Skills available" dumped every turn).
		for _, blk := range extractPreamblePayloads(pa.req.body) {
			key := preambleSignature(blk.Name, blk.Body)
			prow := upsertRow(preambles, key)
			prow.Requests++
			prow.InputTokens += int64(len(blk.Body))
		}

		// Content breakdown: re-parse the request body via
		// gateway.SectionBytes and accumulate per-section bytes.
		// This is the on-read path for input_bytes per SPEC §4.6.
		// Skipped when the body is empty (metadata-level log) or
		// when direction is missing — both signal "no body to
		// classify".
		if len(pa.req.body) > 0 {
			clientAPI := clientAPIFromDirection(pa.req.direction)
			if clientAPI != "" {
				sb := cachedSectionBytes(pairKey{id: pa.req.id, run: pa.req.run}, pa.req.body, clientAPI)
				resp.Totals.ContentBreakdown.System += int64(sb.System)
				resp.Totals.ContentBreakdown.Tools += int64(sb.Tools)
				resp.Totals.ContentBreakdown.UserText += int64(sb.UserText)
				resp.Totals.ContentBreakdown.Preamble += int64(sb.Preamble)
				resp.Totals.ContentBreakdown.ToolResults += int64(sb.ToolResults)
				resp.Totals.ContentBreakdown.Thinking += int64(sb.Thinking)
				resp.Totals.ContentBreakdown.ImagesWire += int64(sb.ImagesWire)
				resp.Totals.ContentBreakdown.UserHistory += int64(sb.UserHistory)
				resp.Totals.ContentBreakdown.Other += int64(sb.Other)
				resp.Totals.ContentBreakdown.Total += int64(sb.Total())
				resp.Totals.ContentBreakdown.Rows++
			}
		}

		if pa.haveResp {
			total := int64(pa.resp.inputTokens) + int64(pa.resp.outputTokens) +
				int64(pa.resp.cacheReadTokens) + int64(pa.resp.cacheCreateTokens)
			if total > 0 {
				topCandidates = append(topCandidates, auditStatsTopRequest{
					ID:           pa.req.id,
					Run:          pa.req.run,
					TS:           pa.req.ts.UTC().Format(time.RFC3339),
					Model:        pa.req.model,
					Session:      pa.req.sessionID,
					Path:         pa.req.path,
					InputTokens:  int64(pa.resp.inputTokens) + int64(pa.resp.cacheReadTokens) + int64(pa.resp.cacheCreateTokens),
					OutputTokens: int64(pa.resp.outputTokens),
					TotalTokens:  total,
				})
			}
		}

		if f.bucket > 0 && !pa.req.ts.IsZero() {
			b := pa.req.ts.Truncate(f.bucket)
			bucket, ok := series[b]
			if !ok {
				bucket = &auditStatsBucket{T: b.UTC().Format(time.RFC3339)}
				series[b] = bucket
			}
			bucket.Requests++
			if pa.haveResp {
				bucket.InputTokens += int64(pa.resp.inputTokens)
				bucket.OutputTokens += int64(pa.resp.outputTokens)
				bucket.CacheReadTokens += int64(pa.resp.cacheReadTokens)
				bucket.CacheCreateTokens += int64(pa.resp.cacheCreateTokens)
			}
		}
	}

	totalIn := resp.Totals.InputTokens + resp.Totals.CacheReadTokens + resp.Totals.CacheCreateTokens
	if totalIn > 0 {
		resp.Totals.CacheHitRatio = float64(resp.Totals.CacheReadTokens) / float64(totalIn)
	}

	resp.ByModel = sortedRows(models)
	resp.BySession = sortedRows(sessions)
	resp.ByPath = sortedRows(paths)
	resp.PreambleBlocks = sortedRows(preambles)

	resp.ByHour = make([]auditStatsHourRow, 0, 24)
	for _, h := range hours {
		resp.ByHour = append(resp.ByHour, *h)
	}
	sort.Slice(resp.ByHour, func(i, j int) bool { return resp.ByHour[i].Hour < resp.ByHour[j].Hour })

	sort.Slice(topCandidates, func(i, j int) bool {
		return topCandidates[i].TotalTokens > topCandidates[j].TotalTokens
	})
	if len(topCandidates) > 20 {
		topCandidates = topCandidates[:20]
	}
	resp.TopRequests = topCandidates

	resp.Series = make([]auditStatsBucket, 0, len(series))
	for _, b := range series {
		resp.Series = append(resp.Series, *b)
	}
	sort.Slice(resp.Series, func(i, j int) bool {
		return resp.Series[i].T < resp.Series[j].T
	})

	return resp, nil
}

// statsPair groups the request and response halves of an audit pair while
// computeAuditStats walks files. Either half may be missing when the log was
// truncated.
type statsPair struct {
	req, resp auditStatsRowSource
	haveReq   bool
	haveResp  bool
}

type auditStatsRowSource struct {
	t                 string
	id                uint64
	run               string
	ts                time.Time
	model             string
	sessionID         string
	turnIndex         int
	outcome           string
	status            int
	inputTokens       int
	outputTokens      int
	cacheReadTokens   int
	cacheCreateTokens int
	reasoningTokens   int
	elapsedMs         int64
	path              string // only on req rows
	body              []byte // only on req rows, for preamble block extraction
	direction         string // only on req rows; "a2a"/"a2o"/"o2a"/"o2o"
}

func parseStatsRow(line []byte) (auditStatsRowSource, bool) {
	var minimal struct {
		T         string          `json:"t"`
		ID        uint64          `json:"id"`
		Run       string          `json:"run"`
		TS        string          `json:"ts"`
		Path      string          `json:"path"`
		Direction string          `json:"direction"`
		Model     string          `json:"model"`
		SessionID string          `json:"session_id"`
		TurnIndex int             `json:"turn_index"`
		Status    int             `json:"status"`
		Outcome   string          `json:"outcome"`
		ElapsedMs int64           `json:"elapsed_ms"`
		Body      json.RawMessage `json:"body"`
		Usage     *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			ReasoningTokens          int `json:"reasoning_tokens"`
		} `json:"usage"`
		Summary *struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				ReasoningTokens          int `json:"reasoning_tokens"`
			} `json:"usage"`
		} `json:"stream_summary"`
	}
	if err := json.Unmarshal(line, &minimal); err != nil {
		return auditStatsRowSource{}, false
	}
	row := auditStatsRowSource{
		t:         minimal.T,
		id:        minimal.ID,
		run:       minimal.Run,
		model:     minimal.Model,
		sessionID: minimal.SessionID,
		turnIndex: minimal.TurnIndex,
		status:    minimal.Status,
		outcome:   minimal.Outcome,
		elapsedMs: minimal.ElapsedMs,
		path:      minimal.Path,
		body:      minimal.Body,
		direction: minimal.Direction,
	}
	if t, err := time.Parse(time.RFC3339Nano, minimal.TS); err == nil {
		row.ts = t
	}
	switch {
	case minimal.Usage != nil:
		row.inputTokens = minimal.Usage.InputTokens
		row.outputTokens = minimal.Usage.OutputTokens
		row.cacheReadTokens = minimal.Usage.CacheReadInputTokens
		row.cacheCreateTokens = minimal.Usage.CacheCreationInputTokens
		row.reasoningTokens = minimal.Usage.ReasoningTokens
	case minimal.Summary != nil && minimal.Summary.Usage != nil:
		row.inputTokens = minimal.Summary.Usage.InputTokens
		row.outputTokens = minimal.Summary.Usage.OutputTokens
		row.cacheReadTokens = minimal.Summary.Usage.CacheReadInputTokens
		row.cacheCreateTokens = minimal.Summary.Usage.CacheCreationInputTokens
		row.reasoningTokens = minimal.Summary.Usage.ReasoningTokens
	}
	return row, true
}

// containsCSV reports whether needle appears as a comma-joined entry in csv.
// Exact match only — "foo/bar" does not match "foo/bar/baz".
func containsCSV(csv, needle string) bool {
	if csv == "" {
		return false
	}
	for i := 0; i < len(csv); {
		j := i
		for j < len(csv) && csv[j] != ',' {
			j++
		}
		entry := csv[i:j]
		if len(entry) > 0 && entry[0] == ' ' {
			entry = entry[1:]
		}
		if entry == needle {
			return true
		}
		if j < len(csv) {
			j++
		}
		i = j
	}
	return false
}

func upsertRow(m map[string]*auditStatsRow, key string) *auditStatsRow {
	if r, ok := m[key]; ok {
		return r
	}
	r := &auditStatsRow{Key: key}
	m[key] = r
	return r
}

func applyPair(row *auditStatsRow, pa *statsPair) {
	row.Requests++
	if pa.haveResp {
		row.InputTokens += int64(pa.resp.inputTokens)
		row.OutputTokens += int64(pa.resp.outputTokens)
		row.CacheReadTokens += int64(pa.resp.cacheReadTokens)
		row.CacheCreateTokens += int64(pa.resp.cacheCreateTokens)
	}
	if !pa.req.ts.IsZero() {
		ts := pa.req.ts.UTC().Format(time.RFC3339)
		if row.FirstSeen == "" || ts < row.FirstSeen {
			row.FirstSeen = ts
		}
		if ts > row.LastSeen {
			row.LastSeen = ts
		}
	}
}

func sortedRows(m map[string]*auditStatsRow) []auditStatsRow {
	out := make([]auditStatsRow, 0, len(m))
	for _, r := range m {
		if r.Key == "" {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].InputTokens + out[i].OutputTokens
		tj := out[j].InputTokens + out[j].OutputTokens
		if ti != tj {
			return ti > tj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func formatWindow(since, until time.Time) string {
	if since.IsZero() && until.IsZero() {
		return "all"
	}
	return since.UTC().Format(time.RFC3339) + "/" + until.UTC().Format(time.RFC3339)
}

// parseWindowParam translates "1h" / "24h" / "7d" / "30d" / "all" into a
// (since, until) pair. "all" returns the zero value for both, which the stats
// walker interprets as "no time bound". Bare RFC3339 timestamps are rejected
// here on purpose — callers use the explicit since/until query params for
// that.
func parseWindowParam(window string, now time.Time) (since, until time.Time) {
	until = now
	switch window {
	case "", "all":
		return time.Time{}, time.Time{}
	case "1h":
		return now.Add(-time.Hour), until
	case "24h":
		return now.Add(-24 * time.Hour), until
	case "7d":
		return now.Add(-7 * 24 * time.Hour), until
	case "30d":
		return now.Add(-30 * 24 * time.Hour), until
	}
	if d, err := time.ParseDuration(window); err == nil && d > 0 {
		return now.Add(-d), until
	}
	return time.Time{}, time.Time{}
}

// sectionBytesCache memoizes gateway.SectionBytes results across
// admin-stats fetches. Audit log rows are immutable once written, so
// a (run, id) tuple uniquely and permanently identifies the body
// — caching the partition is safe forever for the process lifetime.
//
// Why we need this: per SPEC §4.6, input_bytes is computed on read
// rather than persisted on disk. Without caching, every admin UI
// fetch re-parses every row's body — measured at ~60s+ for the
// real ~/.mesh/gateway/claude-audit dir (~1 GB across 14 files).
// With the cache, the second-and-later fetches are sub-50ms; the
// first cold fetch still pays the parse cost but only once per
// process.
//
// Sized: unbounded sync.Map. At ~200 B per entry × 100K rows ≈
// 20 MB; audit retention bounds growth at the source. If a
// future workload pushes that past comfort, switch to a bounded
// LRU — but the §4.6 reversal (persist input_bytes on disk)
// becomes the better answer first.
var sectionBytesCache sync.Map // pairKey -> gateway.SectionByteCounts

// cachedSectionBytes is the read-time wrapper around gateway.SectionBytes
// that memoizes per audit row. Misses populate the cache; hits skip the
// parse entirely. Concurrency-safe via sync.Map.
func cachedSectionBytes(key pairKey, body []byte, clientAPI string) gateway.SectionByteCounts {
	if cached, ok := sectionBytesCache.Load(key); ok {
		return cached.(gateway.SectionByteCounts)
	}
	sb := gateway.SectionBytes(body, clientAPI)
	sectionBytesCache.Store(key, sb)
	return sb
}

// clientAPIFromDirection maps the audit row's direction tag back to
// the client-side API name SectionBytes expects. Returns "" for
// missing or unrecognized directions so callers skip the breakdown
// rather than misclassify.
func clientAPIFromDirection(dir string) string {
	if dir == "" {
		return ""
	}
	switch dir[0] {
	case 'a':
		return gateway.APIAnthropic
	case 'o':
		return gateway.APIOpenAI
	}
	return ""
}

// extractPreamblePayloads walks the user-role messages in a request body
// and returns every pseudo-XML preamble block (`<system-reminder>`,
// `<command-name>`, etc.) at the leading/trailing edges of typed
// content. Delegates the per-string parsing to gateway.ExtractPreambleBlocks
// — the canonical extractor used by the section-byte partition and
// the admin-stats rollup. Drift between cmd/mesh and gateway-side
// behavior is policed by internal/gateway/preamble_drift_test.go.
//
// Tolerates any input shape: Anthropic string content, Anthropic
// content-block array, OpenAI inline string. Non-user messages are
// skipped because they are the assistant's or tools' output, not
// preamble.
func extractPreamblePayloads(bodyJSON []byte) []gateway.PreambleBlock {
	if len(bodyJSON) == 0 {
		return nil
	}
	var shell struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyJSON, &shell); err != nil {
		return nil
	}
	var out []gateway.PreambleBlock
	for _, m := range shell.Messages {
		if m.Role != "user" {
			continue
		}
		var asStr string
		if err := json.Unmarshal(m.Content, &asStr); err == nil {
			out = append(out, gateway.ExtractPreambleBlocks(asStr)...)
			continue
		}
		var asArr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &asArr); err == nil {
			for _, b := range asArr {
				if b.Type == "text" {
					out = append(out, gateway.ExtractPreambleBlocks(b.Text)...)
				}
			}
		}
	}
	return out
}

// preambleSignature collapses long matching blocks into one aggregation
// bucket: tag name + first 40 collapsed-whitespace chars. Two blocks with
// the same leading text aggregate together, so the UI answers "the Skills
// reminder is showing up in 90% of requests, totalling 4.2 MB" instead of
// fracturing by punctuation noise.
func preambleSignature(name, body string) string {
	// Collapse runs of whitespace to a single space, trim.
	var b strings.Builder
	prevSpace := true
	b.Grow(len(body))
	for _, r := range body {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	sig := strings.TrimSpace(b.String())
	const maxSig = 60
	if len(sig) > maxSig {
		sig = sig[:maxSig] + "…"
	}
	return "<" + name + "> " + sig
}

// extractProjectPath scans a request body JSON for a "Primary working
// directory:" line injected by Claude Code into the system prompt. Returns the
// last two path segments of that directory (e.g. "mmdemirbas/mesh") so the
// "By project" breakdown shows meaningful names instead of URL paths.
// Returns "" when the body is absent or carries no working-directory hint.
func extractProjectPath(bodyJSON []byte) string {
	if len(bodyJSON) == 0 {
		return ""
	}
	// The working-directory line appears in the top-level "system" field
	// (Anthropic format) or as the content of a system-role message.
	var shell struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodyJSON, &shell); err != nil {
		return ""
	}
	candidates := make([]string, 0, 4)
	// system field may be a plain string or an array of content blocks.
	if len(shell.System) > 0 {
		var s string
		if json.Unmarshal(shell.System, &s) == nil {
			candidates = append(candidates, s)
		} else {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(shell.System, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						candidates = append(candidates, b.Text)
					}
				}
			}
		}
	}
	for _, m := range shell.Messages {
		if m.Role != "system" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			candidates = append(candidates, s)
		}
	}
	const marker = "Primary working directory: "
	for _, text := range candidates {
		_, after, found := strings.Cut(text, marker)
		if !found {
			continue
		}
		line, _, _ := strings.Cut(after, "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Return last two non-empty path segments for concise display.
		line = filepath.ToSlash(line)
		rawParts := strings.Split(strings.TrimRight(line, "/"), "/")
		var parts []string
		for _, p := range rawParts {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
		return parts[len(parts)-1]
	}
	return ""
}

// parseBucketParam maps "minute" | "hour" | "day" to a duration. Empty or
// unknown returns a zero duration, which disables the time series.
func parseBucketParam(bucket string) time.Duration {
	switch bucket {
	case "minute":
		return time.Minute
	case "hour":
		return time.Hour
	case "day":
		return 24 * time.Hour
	}
	return 0
}
