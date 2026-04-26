package main

import (
	"encoding/json"
	"sync"
)

// B5 cache_control visibility — extraction + aggregation.
//
// Two data sources, both already in audit rows:
//
//   - Request side: cache_control markers in request bodies (Anthropic).
//     extractCacheMarkers walks system blocks, tools, and message
//     content blocks counting markers. OpenAI bodies have no
//     cache_control concept; extraction returns zero markers.
//   - Response side: usage.cache_read_input_tokens and
//     cache_creation_input_tokens (Anthropic) or
//     prompt_cache_hit_tokens (OpenAI). extractCacheUsage parses by
//     source per gateway direction.
//
// All parsing is read-time, memoized per (run, id) tuple via
// cacheMarkersCache and cacheUsageCache. No write-time changes to
// audit.go; existing logs work without migration.
//
// See docs/gateway/DESIGN_B5_cache_visibility.local.md.

// CacheMarkers is the categorized cache_control marker count for one
// request body. Total > 0 means "cache_control is being used"; the
// sub-counts indicate where (per design §1.1).
type CacheMarkers struct {
	OnSystem        bool `json:"on_system"`
	OnToolCount     int  `json:"on_tool_count"`
	ToolTotalCount  int  `json:"tool_total_count"`
	OnMessageCount  int  `json:"on_message_count"`
	Total           int  `json:"total"`
}

// CacheUsage is the per-row cache token accounting derived from the
// response body's usage block. HitRate is in [0, 1] when cache fields
// were reported, or -1 when the upstream did not surface caching at
// all (graceful degradation, design D4).
type CacheUsage struct {
	InputTokens         int     `json:"input_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	HitRate             float64 `json:"hit_rate"`
	Source              string  `json:"source"` // "anthropic" | "openai" | "unknown"
}

// CacheSummary is the compact per-node payload surfaced via the SSE
// event and the requests table. nil-safe consumers treat absence as
// "no cache data" (rendered as a dim dash). Frontend reads
// MarkerPresent and HitRate to pick a badge color.
type CacheSummary struct {
	MarkerPresent bool    `json:"marker_present"`
	HitRate       float64 `json:"hit_rate"` // -1 = no data; ≥0 = real ratio
	Source        string  `json:"source"`
	CacheRead     int     `json:"cache_read"`
	CacheCreation int     `json:"cache_creation"`
	StandardInput int     `json:"standard_input"`
}

// extractCacheMarkers parses an Anthropic request body and counts
// cache_control markers across system blocks, tool definitions, and
// message content blocks. Tolerant of unexpected shapes — an OpenAI
// or malformed body returns zero markers without an error.
//
// Edge cases:
//   - system as string (not array): no markers possible per Anthropic
//     API; returns false.
//   - tools missing or empty: ToolTotalCount = 0, OnToolCount = 0.
//   - messages missing: OnMessageCount = 0.
//   - body fails JSON parse: returns the error so the caller can
//     skip caching for this row.
func extractCacheMarkers(body []byte) (CacheMarkers, error) {
	var raw struct {
		System   json.RawMessage   `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return CacheMarkers{}, err
	}

	var m CacheMarkers

	// System: only an array form can carry markers. The string form
	// (which Anthropic also accepts) cannot be marked per spec.
	if firstNonSpace(raw.System) == '[' {
		var blocks []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(raw.System, &blocks) == nil {
			for _, b := range blocks {
				if len(b.CacheControl) > 0 {
					m.OnSystem = true
					break
				}
			}
		}
	}

	// Tools: one fraction (cached / total) per the §1.1 "5 of 5" UX.
	m.ToolTotalCount = len(raw.Tools)
	for _, t := range raw.Tools {
		var tool struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(t, &tool) == nil && len(tool.CacheControl) > 0 {
			m.OnToolCount++
		}
	}

	// Messages: walk content blocks (string-form content has no
	// markers possible).
	for _, msg := range raw.Messages {
		if firstNonSpace(msg.Content) != '[' {
			continue
		}
		var blocks []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if len(b.CacheControl) > 0 {
				m.OnMessageCount++
			}
		}
	}

	if m.OnSystem {
		m.Total++
	}
	m.Total += m.OnToolCount + m.OnMessageCount
	return m, nil
}

// extractCacheUsage parses a response body's usage block. The source
// argument selects the parsing schema; pass "anthropic" or "openai"
// based on the row's direction (first char of "a2o" / "o2a" /
// "a2a" / "o2o" is the client API, which determines body shape in
// the audit row since wrapAuditing tees the client-facing bytes).
//
// Returns HitRate = -1 when no cache fields are present in the
// response (graceful-degradation path, D4). To distinguish "field
// missing" from "field zero", the per-source schema uses pointer
// types — nil for absent, non-nil with zero value for zero-but-
// reported.
func extractCacheUsage(respBody []byte, source string) CacheUsage {
	u := CacheUsage{HitRate: -1, Source: source}
	if len(respBody) == 0 {
		u.Source = "unknown"
		return u
	}
	switch source {
	case "anthropic":
		var resp struct {
			Usage struct {
				InputTokens         *int `json:"input_tokens"`
				CacheReadTokens     *int `json:"cache_read_input_tokens"`
				CacheCreationTokens *int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &resp) != nil {
			return u
		}
		if resp.Usage.InputTokens != nil {
			u.InputTokens = *resp.Usage.InputTokens
		}
		// Cache reporting is "active" when at least one cache field
		// is present in the JSON, regardless of whether the value is
		// zero. The pointer-type unmarshal lets us tell apart.
		hasCache := resp.Usage.CacheReadTokens != nil || resp.Usage.CacheCreationTokens != nil
		if resp.Usage.CacheReadTokens != nil {
			u.CacheReadTokens = *resp.Usage.CacheReadTokens
		}
		if resp.Usage.CacheCreationTokens != nil {
			u.CacheCreationTokens = *resp.Usage.CacheCreationTokens
		}
		if hasCache {
			denom := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
			if denom > 0 {
				u.HitRate = float64(u.CacheReadTokens) / float64(denom)
			} else {
				u.HitRate = 0
			}
		}

	case "openai":
		var resp struct {
			Usage struct {
				PromptTokens         *int `json:"prompt_tokens"`
				PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &resp) != nil {
			return u
		}
		if resp.Usage.PromptCacheHitTokens != nil {
			u.CacheReadTokens = *resp.Usage.PromptCacheHitTokens
		}
		if resp.Usage.PromptTokens != nil {
			pt := *resp.Usage.PromptTokens
			u.InputTokens = pt - u.CacheReadTokens
			if u.InputTokens < 0 {
				u.InputTokens = 0
			}
			if resp.Usage.PromptCacheHitTokens != nil {
				if pt > 0 {
					u.HitRate = float64(u.CacheReadTokens) / float64(pt)
				} else {
					u.HitRate = 0
				}
			}
		}

	default:
		u.Source = "unknown"
	}
	return u
}

// CacheSummaryFrom collapses a per-row Markers + Usage pair into the
// compact frontend-facing summary. Returns nil when neither side has
// usable data (frontend renders "no cache data"). The MarkerPresent
// flag is true only when at least one cache_control marker was found
// in the request body.
func CacheSummaryFrom(markers *CacheMarkers, usage *CacheUsage) *CacheSummary {
	hasMarkers := markers != nil && markers.Total > 0
	hasUsage := usage != nil && usage.HitRate >= 0
	if !hasMarkers && !hasUsage {
		return nil
	}
	cs := &CacheSummary{HitRate: -1, Source: "unknown"}
	if markers != nil {
		cs.MarkerPresent = markers.Total > 0
	}
	if usage != nil {
		cs.HitRate = usage.HitRate
		cs.CacheRead = usage.CacheReadTokens
		cs.CacheCreation = usage.CacheCreationTokens
		cs.StandardInput = usage.InputTokens
		if usage.Source != "" {
			cs.Source = usage.Source
		}
	}
	return cs
}

// AggregateCacheStats is the overview-card payload — totals and
// counts across a window of rows.
type AggregateCacheStats struct {
	HitRate                float64 `json:"hit_rate"` // -1 when no cache data in window
	TotalCacheRead         int     `json:"total_cache_read_tokens"`
	TotalCacheCreation     int     `json:"total_cache_creation_tokens"`
	TotalStandardInput     int     `json:"total_standard_input_tokens"`
	RowsWithCacheData      int     `json:"rows_with_cache_data"`
	RowsTotal              int     `json:"rows_total"`
	RowsWithMarkers        int     `json:"rows_with_markers"`
	RowsWithSystemMarker   int     `json:"rows_with_system_marker"`
	RowsWithToolMarker     int     `json:"rows_with_tool_marker"`
	RowsWithMessageMarker  int     `json:"rows_with_message_marker"`
}

// cacheAccumulator is the streaming aggregator used during a row
// scan. AddRow handles each (markers, usage) pair; Finish returns
// the AggregateCacheStats with HitRate computed once.
type cacheAccumulator struct {
	totalRead     int
	totalCreation int
	totalInput    int
	rowsWithData  int
	rowsTotal     int
	rowsMarkers   int
	rowsSystem    int
	rowsTool      int
	rowsMessage   int
}

// AddRow folds one row's per-row markers+usage into the running
// totals. nil markers/usage are valid (mean "no cache data on this
// row") and just bump rowsTotal.
func (a *cacheAccumulator) AddRow(markers *CacheMarkers, usage *CacheUsage) {
	a.rowsTotal++
	if markers != nil && markers.Total > 0 {
		a.rowsMarkers++
		if markers.OnSystem {
			a.rowsSystem++
		}
		if markers.OnToolCount > 0 {
			a.rowsTool++
		}
		if markers.OnMessageCount > 0 {
			a.rowsMessage++
		}
	}
	if usage != nil && usage.HitRate >= 0 {
		a.rowsWithData++
		a.totalRead += usage.CacheReadTokens
		a.totalCreation += usage.CacheCreationTokens
		a.totalInput += usage.InputTokens
	}
}

func (a *cacheAccumulator) Finish() AggregateCacheStats {
	out := AggregateCacheStats{
		HitRate:               -1,
		TotalCacheRead:        a.totalRead,
		TotalCacheCreation:    a.totalCreation,
		TotalStandardInput:    a.totalInput,
		RowsWithCacheData:     a.rowsWithData,
		RowsTotal:             a.rowsTotal,
		RowsWithMarkers:       a.rowsMarkers,
		RowsWithSystemMarker:  a.rowsSystem,
		RowsWithToolMarker:    a.rowsTool,
		RowsWithMessageMarker: a.rowsMessage,
	}
	denom := a.totalInput + a.totalCreation + a.totalRead
	if denom > 0 {
		out.HitRate = float64(a.totalRead) / float64(denom)
	}
	return out
}

// --- Read-time per-row caches ---
//
// Same idea as sectionBytesCache in audit_stats.go: audit rows are
// immutable once written, so a (run, id) tuple permanently identifies
// a body's parse result. Caching avoids re-parsing on every admin
// fetch. Sized: unbounded sync.Map (audit retention bounds growth).

// cacheMarkersCache memoizes per-request CacheMarkers extraction.
// Keys: pairKey. Values: *CacheMarkers (nil for parse-failed rows
// stored as a sentinel so we don't retry).
var cacheMarkersCache sync.Map

// cacheUsageCache memoizes per-response CacheUsage extraction. Keys:
// pairKey. Values: *CacheUsage (always non-nil; a nil result of
// extraction is impossible — extractCacheUsage always returns a
// CacheUsage with HitRate = -1 sentinel).
var cacheUsageCache sync.Map

// cachedCacheMarkers wraps extractCacheMarkers with the per-(run,id)
// cache. Returns the cached result (nil when prior parse failed) so
// callers can branch.
func cachedCacheMarkers(key pairKey, body []byte) *CacheMarkers {
	if v, ok := cacheMarkersCache.Load(key); ok {
		if v == nil {
			return nil
		}
		return v.(*CacheMarkers)
	}
	m, err := extractCacheMarkers(body)
	if err != nil {
		cacheMarkersCache.Store(key, (*CacheMarkers)(nil))
		return nil
	}
	cacheMarkersCache.Store(key, &m)
	return &m
}

// cachedCacheUsage wraps extractCacheUsage with the per-(run,id)
// cache. Always returns a non-nil pointer; HitRate < 0 indicates
// "no cache fields in response."
func cachedCacheUsage(key pairKey, respBody []byte, source string) *CacheUsage {
	if v, ok := cacheUsageCache.Load(key); ok {
		return v.(*CacheUsage)
	}
	u := extractCacheUsage(respBody, source)
	cacheUsageCache.Store(key, &u)
	return &u
}

// directionToSource maps the audit row's direction tag to the
// source argument extractCacheUsage expects. The first char of the
// direction is the client API: 'a' → anthropic, 'o' → openai.
// wrapAuditing tees the client-facing bytes into the audit row's
// body field, so the body shape always matches the client API
// regardless of upstream.
func directionToSource(direction string) string {
	if direction == "" {
		return "unknown"
	}
	switch direction[0] {
	case 'a':
		return "anthropic"
	case 'o':
		return "openai"
	}
	return "unknown"
}

// firstNonSpace returns the first non-whitespace byte of b, or 0 if
// b is empty or all-whitespace. Used to distinguish JSON object,
// array, and string literal forms before a full unmarshal.
func firstNonSpace(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c
	}
	return 0
}
