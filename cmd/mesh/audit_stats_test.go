package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
)

// writeStatsFixture seeds an audit dir with three pairs designed to exercise
// every aggregation path: two opus calls in one session (one cached, one
// fresh) and one error sonnet call in a different session. Returns the
// gateway-namespaced sub-directory that holds the JSONL files (NewRecorder
// always nests under <dir>/<name>).
func writeStatsFixture(t *testing.T, dir, name string) string {
	t.Helper()
	cfg := gateway.GatewayCfg{
		Name:        name,
		Bind:        "127.0.0.1:0",
		Upstream:    "https://api.anthropic.com",
		ClientAPI:   gateway.APIAnthropic,
		UpstreamAPI: gateway.APIAnthropic,
		Log:         gateway.LogCfg{Level: gateway.LogLevelMetadata, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// Pair 1: opus, session sess-1, cached prompt.
	id1 := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-1", TurnIndex: 1,
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec.Response(id1, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		Usage: &gateway.Usage{
			InputTokens: 100, OutputTokens: 50,
			CacheReadInputTokens: 900, CacheCreationInputTokens: 0,
		},
		StartTime: time.Now(), EndTime: time.Now().Add(120 * time.Millisecond),
	}, nil)

	// Pair 2: opus, session sess-1 turn 2, fresh prompt with cache write.
	id2 := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-1", TurnIndex: 3,
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hey"},{"role":"user","content":"more"}]}`))
	rec.Response(id2, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		Usage: &gateway.Usage{
			InputTokens: 200, OutputTokens: 80,
			CacheReadInputTokens: 0, CacheCreationInputTokens: 1000,
		},
		StartTime: time.Now(), EndTime: time.Now().Add(150 * time.Millisecond),
	}, nil)

	// Pair 3: sonnet, sess-2, error.
	id3 := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-2", TurnIndex: 1,
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"yo"}]}`))
	rec.Response(id3, gateway.ResponseMeta{
		Status: 500, Outcome: gateway.OutcomeError,
		Usage:     &gateway.Usage{InputTokens: 5, OutputTokens: 0},
		StartTime: time.Now(), EndTime: time.Now().Add(50 * time.Millisecond),
	}, nil)
	return filepath.Join(dir, name)
}

// TestAuditStatsTotals verifies the totals struct: request count, error count,
// per-bucket token sums, and the precomputed cache hit ratio.
func TestAuditStatsTotals(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-totals")
	stats, err := computeAuditStats(auditDir, statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if stats.Totals.Requests != 3 {
		t.Errorf("requests = %d, want 3", stats.Totals.Requests)
	}
	if stats.Totals.Errors != 1 {
		t.Errorf("errors = %d, want 1", stats.Totals.Errors)
	}
	if stats.Totals.InputTokens != 305 {
		t.Errorf("input = %d, want 305", stats.Totals.InputTokens)
	}
	if stats.Totals.OutputTokens != 130 {
		t.Errorf("output = %d, want 130", stats.Totals.OutputTokens)
	}
	if stats.Totals.CacheReadTokens != 900 {
		t.Errorf("cache_read = %d, want 900", stats.Totals.CacheReadTokens)
	}
	if stats.Totals.CacheCreateTokens != 1000 {
		t.Errorf("cache_creation = %d, want 1000", stats.Totals.CacheCreateTokens)
	}
	// 900 / (305 + 900 + 1000) = 0.4081…
	if r := stats.Totals.CacheHitRatio; r < 0.40 || r > 0.42 {
		t.Errorf("cache_hit_ratio = %.4f, want ~0.408", r)
	}
}

// TestAuditStatsBreakdowns confirms by_model and by_session aggregation,
// including FirstModel and Turns on session rows.
func TestAuditStatsBreakdowns(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-breakdowns")
	stats, err := computeAuditStats(auditDir, statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	models := map[string]auditStatsRow{}
	for _, m := range stats.ByModel {
		models[m.Key] = m
	}
	if len(models) != 2 {
		t.Errorf("by_model count = %d, want 2", len(models))
	}
	opus := models["claude-opus-4-6"]
	if opus.Requests != 2 {
		t.Errorf("opus requests = %d", opus.Requests)
	}
	if opus.InputTokens != 300 || opus.OutputTokens != 130 {
		t.Errorf("opus tokens = %d/%d", opus.InputTokens, opus.OutputTokens)
	}

	sessions := map[string]auditStatsRow{}
	for _, s := range stats.BySession {
		sessions[s.Key] = s
	}
	s1 := sessions["sess-1"]
	if s1.Requests != 2 {
		t.Errorf("sess-1 requests = %d", s1.Requests)
	}
	if s1.Turns != 3 {
		t.Errorf("sess-1 turns = %d, want 3 (max turn_index)", s1.Turns)
	}
	if s1.FirstModel != "claude-opus-4-6" {
		t.Errorf("sess-1 first_model = %q", s1.FirstModel)
	}
}

// TestAuditStatsFilters verifies session and model filters narrow the totals.
func TestAuditStatsFilters(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-filters")
	got, err := computeAuditStats(auditDir, statsFilter{session: "sess-2"})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got.Totals.Requests != 1 {
		t.Errorf("session filter: requests = %d, want 1", got.Totals.Requests)
	}
	if got.Totals.Errors != 1 {
		t.Errorf("session filter: errors = %d, want 1", got.Totals.Errors)
	}

	gotM, err := computeAuditStats(auditDir, statsFilter{model: "claude-opus-4-6"})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if gotM.Totals.Requests != 2 {
		t.Errorf("model filter: requests = %d, want 2", gotM.Totals.Requests)
	}
}

// TestAuditStatsTimeSeries verifies that bucket=minute produces a non-empty
// series with per-bucket counters.
func TestAuditStatsTimeSeries(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-series")
	got, err := computeAuditStats(auditDir, statsFilter{bucket: time.Minute})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(got.Series) == 0 {
		t.Fatal("expected at least one series bucket")
	}
	var totalReq int
	var totalIn int64
	for _, b := range got.Series {
		totalReq += b.Requests
		totalIn += b.InputTokens
	}
	if totalReq != 3 {
		t.Errorf("series requests = %d, want 3", totalReq)
	}
	if totalIn != 305 {
		t.Errorf("series input = %d, want 305 (sum across buckets)", totalIn)
	}
}

// TestAuditStatsEndpoint exercises GET /api/gateway/audit/stats end-to-end.
func TestAuditStatsEndpoint(t *testing.T) {
	dir := t.TempDir()
	name := "stats-endpoint"
	writeStatsFixture(t, dir, name)

	srv := httptest.NewServer(buildAdminMux(newLogRing(4), ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gateway/audit/stats?gateway=" + name + "&window=24h&bucket=hour")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var stats auditStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.Totals.Requests != 3 {
		t.Errorf("requests = %d, want 3", stats.Totals.Requests)
	}
	if stats.Bucket != "1h0m0s" {
		t.Errorf("bucket echo = %q", stats.Bucket)
	}

	// Missing gateway → 400.
	bad, _ := http.Get(srv.URL + "/api/gateway/audit/stats")
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != 400 {
		t.Errorf("missing gateway status = %d, want 400", bad.StatusCode)
	}

	// Unknown gateway → 404.
	miss, _ := http.Get(srv.URL + "/api/gateway/audit/stats?gateway=does-not-exist")
	defer func() { _ = miss.Body.Close() }()
	if miss.StatusCode != 404 {
		t.Errorf("unknown gateway status = %d, want 404", miss.StatusCode)
	}
}

// TestParseWindowParam covers the canned windows + duration fallback + the
// rejection path that should return zero values.
func TestParseWindowParam(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		input       string
		wantSinceOK bool
	}{
		{"all", "all", false},
		{"empty", "", false},
		{"1h", "1h", true},
		{"24h", "24h", true},
		{"7d", "7d", true},
		{"30d", "30d", true},
		{"duration", "90m", true},
		{"garbage", "yesterday", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := parseWindowParam(tc.input, now)
			if tc.wantSinceOK && s.IsZero() {
				t.Errorf("%s: since unset", tc.input)
			}
			if !tc.wantSinceOK && !s.IsZero() {
				t.Errorf("%s: since unexpectedly set to %v", tc.input, s)
			}
		})
	}
}
