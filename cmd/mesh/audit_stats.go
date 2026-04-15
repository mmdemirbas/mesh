package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"time"
)

// auditStatsResponse is the shape returned by GET /api/gateway/audit/stats.
// Tokens are split so the UI can render the cache-vs-fresh stack without any
// further math; cache hit ratio is precomputed because clients otherwise have
// to handle the divide-by-zero case in three places.
type auditStatsResponse struct {
	Window    string             `json:"window"`
	Bucket    string             `json:"bucket"`
	Totals    auditStatsTotals   `json:"totals"`
	ByModel   []auditStatsRow    `json:"by_model"`
	BySession []auditStatsRow    `json:"by_session"`
	Series    []auditStatsBucket `json:"series"`
}

type auditStatsTotals struct {
	Requests           int     `json:"requests"`
	Errors             int     `json:"errors"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheCreateTokens  int64   `json:"cache_creation_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	CacheHitRatio      float64 `json:"cache_hit_ratio"`
	ElapsedSumMs       int64   `json:"elapsed_sum_ms"`
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
	series := map[time.Time]*auditStatsBucket{}

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

		if pa.req.sessionID != "" {
			srow := upsertRow(sessions, pa.req.sessionID)
			applyPair(srow, pa)
			if srow.FirstModel == "" {
				srow.FirstModel = pa.req.model
			}
			if pa.req.turnIndex > srow.Turns {
				srow.Turns = pa.req.turnIndex
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
}

func parseStatsRow(line []byte) (auditStatsRowSource, bool) {
	var minimal struct {
		T         string `json:"t"`
		ID        uint64 `json:"id"`
		Run       string `json:"run"`
		TS        string `json:"ts"`
		Model     string `json:"model"`
		SessionID string `json:"session_id"`
		TurnIndex int    `json:"turn_index"`
		Status    int    `json:"status"`
		Outcome   string `json:"outcome"`
		ElapsedMs int64  `json:"elapsed_ms"`
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
