package main

import (
	"testing"
)

// --- extractCacheMarkers ---

func TestExtractCacheMarkers_AllSurfaces(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"system": [
			{"type":"text","text":"system prompt","cache_control":{"type":"ephemeral"}}
		],
		"tools": [
			{"name":"a","cache_control":{"type":"ephemeral"}},
			{"name":"b","cache_control":{"type":"ephemeral"}},
			{"name":"c"}
		],
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"hi"},
				{"type":"tool_result","content":"ok","cache_control":{"type":"ephemeral"}}
			]},
			{"role":"assistant","content":"plain text reply"}
		]
	}`)
	m, err := extractCacheMarkers(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !m.OnSystem {
		t.Errorf("OnSystem = false, want true")
	}
	if m.OnToolCount != 2 || m.ToolTotalCount != 3 {
		t.Errorf("tools = %d/%d, want 2/3", m.OnToolCount, m.ToolTotalCount)
	}
	if m.OnMessageCount != 1 {
		t.Errorf("OnMessageCount = %d, want 1", m.OnMessageCount)
	}
	if m.Total != 1+2+1 {
		t.Errorf("Total = %d, want 4", m.Total)
	}
}

func TestExtractCacheMarkers_StringSystemNoMarkers(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":"a plain string","tools":[],"messages":[]}`)
	m, err := extractCacheMarkers(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.OnSystem {
		t.Errorf("OnSystem = true on string-form system, want false")
	}
	if m.Total != 0 {
		t.Errorf("Total = %d, want 0", m.Total)
	}
}

func TestExtractCacheMarkers_MissingFieldsAreZero(t *testing.T) {
	t.Parallel()
	// Anthropic body with just messages — no system, no tools.
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	m, err := extractCacheMarkers(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.Total != 0 || m.ToolTotalCount != 0 {
		t.Errorf("expected all zero: %+v", m)
	}
}

func TestExtractCacheMarkers_MalformedReturnsError(t *testing.T) {
	t.Parallel()
	_, err := extractCacheMarkers([]byte(`not json`))
	if err == nil {
		t.Errorf("expected error on malformed body")
	}
}

func TestExtractCacheMarkers_OpenAIBodyHasZero(t *testing.T) {
	t.Parallel()
	// OpenAI request body — no cache_control concept.
	body := []byte(`{"model":"glm","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`)
	m, err := extractCacheMarkers(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.Total != 0 {
		t.Errorf("Total = %d on OpenAI body, want 0", m.Total)
	}
	if m.ToolTotalCount != 1 {
		t.Errorf("ToolTotalCount = %d, want 1", m.ToolTotalCount)
	}
}

// --- extractCacheUsage (anthropic) ---

func TestExtractCacheUsage_AnthropicWithCache(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usage":{"input_tokens":1234,"output_tokens":567,"cache_read_input_tokens":8000,"cache_creation_input_tokens":12000}}`)
	u := extractCacheUsage(body, "anthropic")
	if u.Source != "anthropic" {
		t.Errorf("Source = %q, want anthropic", u.Source)
	}
	if u.InputTokens != 1234 || u.CacheReadTokens != 8000 || u.CacheCreationTokens != 12000 {
		t.Errorf("usage = %+v", u)
	}
	want := 8000.0 / float64(1234+8000+12000)
	if u.HitRate < want-1e-9 || u.HitRate > want+1e-9 {
		t.Errorf("HitRate = %v, want %v", u.HitRate, want)
	}
}

func TestExtractCacheUsage_AnthropicNoCacheFields(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50}}`)
	u := extractCacheUsage(body, "anthropic")
	if u.HitRate != -1 {
		t.Errorf("HitRate = %v, want -1 (sentinel for no cache fields)", u.HitRate)
	}
	if u.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", u.InputTokens)
	}
}

func TestExtractCacheUsage_AnthropicZeroCacheStillReports(t *testing.T) {
	t.Parallel()
	// Cache fields present but both zero — first request that hasn't
	// hit cache yet. Hit rate should be 0 (real zero), not -1.
	body := []byte(`{"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
	u := extractCacheUsage(body, "anthropic")
	if u.HitRate != 0 {
		t.Errorf("HitRate = %v, want 0 (real zero, not sentinel)", u.HitRate)
	}
}

func TestExtractCacheUsage_AnthropicCreationOnlyZeroHit(t *testing.T) {
	t.Parallel()
	// First request of a session: wrote cache, no read. Hit rate 0.
	body := []byte(`{"usage":{"input_tokens":1000,"cache_creation_input_tokens":5000,"cache_read_input_tokens":0}}`)
	u := extractCacheUsage(body, "anthropic")
	if u.HitRate != 0 {
		t.Errorf("HitRate = %v, want 0", u.HitRate)
	}
	if u.CacheCreationTokens != 5000 {
		t.Errorf("CacheCreationTokens = %d, want 5000", u.CacheCreationTokens)
	}
}

// --- extractCacheUsage (openai) ---

func TestExtractCacheUsage_OpenAIWithCache(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":300,"prompt_cache_hit_tokens":700}}`)
	u := extractCacheUsage(body, "openai")
	if u.Source != "openai" {
		t.Errorf("Source = %q, want openai", u.Source)
	}
	if u.CacheReadTokens != 700 {
		t.Errorf("CacheReadTokens = %d, want 700", u.CacheReadTokens)
	}
	if u.InputTokens != 300 { // prompt_tokens - cache_hit
		t.Errorf("InputTokens = %d, want 300", u.InputTokens)
	}
	want := 0.7
	if u.HitRate < want-1e-9 || u.HitRate > want+1e-9 {
		t.Errorf("HitRate = %v, want %v", u.HitRate, want)
	}
}

func TestExtractCacheUsage_OpenAINoCacheField(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":300}}`)
	u := extractCacheUsage(body, "openai")
	if u.HitRate != -1 {
		t.Errorf("HitRate = %v, want -1", u.HitRate)
	}
}

func TestExtractCacheUsage_UnknownSource(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usage":{"input_tokens":100}}`)
	u := extractCacheUsage(body, "unknown")
	if u.HitRate != -1 || u.Source != "unknown" {
		t.Errorf("expected -1/unknown, got %+v", u)
	}
}

func TestExtractCacheUsage_EmptyBody(t *testing.T) {
	t.Parallel()
	u := extractCacheUsage(nil, "anthropic")
	if u.Source != "unknown" || u.HitRate != -1 {
		t.Errorf("empty body should yield unknown/-1, got %+v", u)
	}
}

// --- CacheSummaryFrom ---

func TestCacheSummaryFrom_NilWhenNeither(t *testing.T) {
	t.Parallel()
	if cs := CacheSummaryFrom(nil, nil); cs != nil {
		t.Errorf("expected nil for nil/nil, got %+v", cs)
	}
}

func TestCacheSummaryFrom_MarkerOnlyAnthropicNoUsage(t *testing.T) {
	t.Parallel()
	m := &CacheMarkers{OnSystem: true, Total: 1}
	cs := CacheSummaryFrom(m, nil)
	if cs == nil {
		t.Fatal("expected non-nil")
	}
	if !cs.MarkerPresent {
		t.Errorf("MarkerPresent = false, want true")
	}
	if cs.HitRate != -1 {
		t.Errorf("HitRate = %v, want -1", cs.HitRate)
	}
}

func TestCacheSummaryFrom_UsageOnlyOpenAI(t *testing.T) {
	t.Parallel()
	u := &CacheUsage{HitRate: 0.4, CacheReadTokens: 400, InputTokens: 600, Source: "openai"}
	cs := CacheSummaryFrom(nil, u)
	if cs == nil {
		t.Fatal("expected non-nil")
	}
	if cs.MarkerPresent {
		t.Errorf("MarkerPresent = true, want false (no markers)")
	}
	if cs.HitRate != 0.4 {
		t.Errorf("HitRate = %v, want 0.4", cs.HitRate)
	}
	if cs.Source != "openai" {
		t.Errorf("Source = %q, want openai", cs.Source)
	}
}

// --- aggregator ---

func TestCacheAccumulator_HitRateAcrossRows(t *testing.T) {
	t.Parallel()
	a := &cacheAccumulator{}
	a.AddRow(&CacheMarkers{OnSystem: true, Total: 1}, &CacheUsage{HitRate: 0.5, CacheReadTokens: 500, CacheCreationTokens: 200, InputTokens: 300})
	a.AddRow(&CacheMarkers{OnToolCount: 2, ToolTotalCount: 3, Total: 2}, &CacheUsage{HitRate: 0.8, CacheReadTokens: 8000, InputTokens: 2000})
	a.AddRow(nil, nil) // no-cache row
	out := a.Finish()
	if out.RowsTotal != 3 {
		t.Errorf("RowsTotal = %d, want 3", out.RowsTotal)
	}
	if out.RowsWithCacheData != 2 {
		t.Errorf("RowsWithCacheData = %d, want 2", out.RowsWithCacheData)
	}
	if out.RowsWithMarkers != 2 {
		t.Errorf("RowsWithMarkers = %d, want 2", out.RowsWithMarkers)
	}
	if out.RowsWithSystemMarker != 1 || out.RowsWithToolMarker != 1 {
		t.Errorf("marker breakdown = %+v", out)
	}
	wantTotalRead := 8500
	if out.TotalCacheRead != wantTotalRead {
		t.Errorf("TotalCacheRead = %d, want %d", out.TotalCacheRead, wantTotalRead)
	}
	denom := 300 + 2000 + 200 + 0 + 500 + 8000
	want := float64(8500) / float64(denom)
	if out.HitRate < want-1e-9 || out.HitRate > want+1e-9 {
		t.Errorf("HitRate = %v, want %v", out.HitRate, want)
	}
}

func TestCacheAccumulator_AllRowsNoCacheGivesSentinelHitRate(t *testing.T) {
	t.Parallel()
	a := &cacheAccumulator{}
	a.AddRow(nil, nil)
	a.AddRow(nil, &CacheUsage{HitRate: -1, Source: "unknown"})
	out := a.Finish()
	if out.HitRate != -1 {
		t.Errorf("HitRate = %v, want -1", out.HitRate)
	}
	if out.RowsTotal != 2 || out.RowsWithCacheData != 0 {
		t.Errorf("counts = %+v", out)
	}
}

// --- direction → source mapping ---

func TestDirectionToSource(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a2o":     "anthropic",
		"a2a":     "anthropic",
		"o2a":     "openai",
		"o2o":     "openai",
		"":        "unknown",
		"unknown": "unknown",
	}
	for dir, want := range cases {
		if got := directionToSource(dir); got != want {
			t.Errorf("directionToSource(%q) = %q, want %q", dir, got, want)
		}
	}
}

// --- per-row caches ---

func TestCachedCacheMarkers_HitsAndMisses(t *testing.T) {
	t.Parallel()
	key := pairKey{id: 9001, run: "test-cache-markers"}
	body := []byte(`{"system":[{"type":"text","cache_control":{"type":"ephemeral"}}]}`)
	first := cachedCacheMarkers(key, body)
	if first == nil || !first.OnSystem {
		t.Fatalf("first call missed: %+v", first)
	}
	second := cachedCacheMarkers(key, []byte(`{"different":"body"}`))
	if second != first {
		t.Errorf("cache miss on second call: %p vs %p", first, second)
	}
}

func TestCachedCacheMarkers_StoresNilOnParseFailure(t *testing.T) {
	t.Parallel()
	key := pairKey{id: 9002, run: "test-cache-markers-fail"}
	if got := cachedCacheMarkers(key, []byte(`bad json`)); got != nil {
		t.Errorf("parse-failed body should yield nil, got %+v", got)
	}
	// Second call must not retry the parse.
	if got := cachedCacheMarkers(key, []byte(`bad json`)); got != nil {
		t.Errorf("nil sentinel not honored on second call")
	}
}

func TestCachedCacheUsage_AlwaysNonNil(t *testing.T) {
	t.Parallel()
	key := pairKey{id: 9003, run: "test-cache-usage"}
	u := cachedCacheUsage(key, []byte(`{"usage":{"input_tokens":100}}`), "anthropic")
	if u == nil {
		t.Fatal("expected non-nil")
	}
	if u.HitRate != -1 {
		t.Errorf("HitRate = %v, want -1 (no cache fields)", u.HitRate)
	}
	// Second call hits cache.
	u2 := cachedCacheUsage(key, []byte(`{"usage":{"input_tokens":200}}`), "anthropic")
	if u2 != u {
		t.Errorf("cache miss")
	}
}
