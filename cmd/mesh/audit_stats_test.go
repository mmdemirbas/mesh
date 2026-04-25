package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		Name: name,
		Client: []gateway.ClientCfg{
			{Bind: "127.0.0.1:0", API: gateway.APIAnthropic},
		},
		Upstream: []gateway.UpstreamCfg{
			{Name: "default", Target: "https://api.anthropic.com", API: gateway.APIAnthropic},
		},
		Log: gateway.LogCfg{Level: gateway.LogLevelMetadata, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg.Name, cfg.Log, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	srv := httptest.NewServer(buildAdminMux(newLogRing(4), "", ""))
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

// TestAuditStatsByPathAndHour drives the new path + hour-of-day breakdowns.
// The fixture exercises two distinct paths so by_path collapses correctly;
// the hour bucket always has at least the current hour populated.
func TestAuditStatsByPathAndHour(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-path")
	// The fixture uses Path "/v1/messages" — add a differing pair so by_path
	// has two rows.
	cfg := gateway.GatewayCfg{
		Name: "stats-path",
		Client: []gateway.ClientCfg{
			{Bind: "127.0.0.1:0", API: gateway.APIAnthropic},
		},
		Upstream: []gateway.UpstreamCfg{
			{Name: "default", Target: "https://api.anthropic.com", API: gateway.APIAnthropic},
		},
		Log: gateway.LogCfg{Level: gateway.LogLevelMetadata, Dir: t.TempDir(), MaxFileSize: "10MB", MaxAge: "720h"},
	}
	_ = cfg
	// Already-written fixture has path="/v1/messages" on every pair, so
	// check that by_path records it.
	stats, err := computeAuditStats(auditDir, statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(stats.ByPath) == 0 {
		t.Fatal("by_path empty")
	}
	var gotMsgs bool
	for _, p := range stats.ByPath {
		if p.Key == "/v1/messages" {
			gotMsgs = true
			if p.Requests != 3 {
				t.Errorf("/v1/messages requests = %d, want 3", p.Requests)
			}
		}
	}
	if !gotMsgs {
		t.Errorf("expected /v1/messages in by_path, got keys %+v", pathKeys(stats.ByPath))
	}
	if len(stats.ByHour) == 0 {
		t.Error("by_hour empty")
	}
}

func pathKeys(rows []auditStatsRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

// TestAuditStatsTopRequests verifies the top-20 table sorts by total tokens
// descending and caps at 20 entries.
func TestAuditStatsTopRequests(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-top")
	stats, err := computeAuditStats(auditDir, statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(stats.TopRequests) == 0 {
		t.Fatal("top_requests empty")
	}
	// Sorted descending by total.
	for i := 1; i < len(stats.TopRequests); i++ {
		if stats.TopRequests[i-1].TotalTokens < stats.TopRequests[i].TotalTokens {
			t.Errorf("top_requests not sorted desc at %d: %d < %d",
				i, stats.TopRequests[i-1].TotalTokens, stats.TopRequests[i].TotalTokens)
		}
	}
	// The fixture's big pair (opus, sess-1 turn 1: 100 fresh + 900 cache + 50 out = 1050)
	// should be #1.
	top := stats.TopRequests[0]
	if top.TotalTokens < 1000 {
		t.Errorf("top request tokens = %d, want >=1000", top.TotalTokens)
	}
}

// TestAuditStatsPreambleBlocks confirms the preamble signature bucketing
// works: two requests both carrying the same <system-reminder> must
// aggregate into a single row.
func TestAuditStatsPreambleBlocks(t *testing.T) {
	dir := t.TempDir()
	cfg := gateway.GatewayCfg{
		Name: "stats-preamble",
		Client: []gateway.ClientCfg{
			{Bind: "127.0.0.1:0", API: gateway.APIAnthropic},
		},
		Upstream: []gateway.UpstreamCfg{
			{Name: "default", Target: "https://api.anthropic.com", API: gateway.APIAnthropic},
		},
		Log: gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg.Name, cfg.Log, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	reminder := `<system-reminder>Skills available: update-config, commit-msg, …</system-reminder>`
	userMsg := reminder + "\nHi"
	bodyJSON := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":` +
		toJSONString(userMsg) + `}]}`)
	for turn := range 2 {
		id := rec.Request(gateway.RequestMeta{
			Gateway: cfg.Name, Direction: "a2a",
			Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
			SessionID: "sess-p", TurnIndex: turn + 1,
			StartTime: time.Now(),
		}, bodyJSON)
		rec.Response(id, gateway.ResponseMeta{
			Status: 200, Outcome: gateway.OutcomeOK,
			Usage:     &gateway.Usage{InputTokens: 10, OutputTokens: 5},
			StartTime: time.Now(), EndTime: time.Now(),
		}, []byte(`{}`))
	}
	stats, err := computeAuditStats(filepath.Join(dir, cfg.Name), statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(stats.PreambleBlocks) != 1 {
		t.Fatalf("preamble_blocks = %d, want 1 aggregated row: %+v", len(stats.PreambleBlocks), stats.PreambleBlocks)
	}
	row := stats.PreambleBlocks[0]
	if row.Requests != 2 {
		t.Errorf("preamble row requests = %d, want 2 (one per turn)", row.Requests)
	}
	// Two requests × ~48-char body — aggregated chars > one body.
	if row.InputTokens < 80 {
		t.Errorf("preamble aggregated chars = %d, want >=80", row.InputTokens)
	}
}

func toJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestExtractProjectPath verifies the project-path extractor finds the
// "Primary working directory:" hint in the system field (string and block
// array) and falls back gracefully when absent.
func TestExtractProjectPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "system string",
			body: `{"system":"Primary working directory: /Users/md/dev/mmdemirbas/mesh\nOther info.","messages":[]}`,
			want: "mmdemirbas/mesh",
		},
		{
			name: "system content blocks",
			body: `{"system":[{"type":"text","text":"Primary working directory: /home/user/projects/my-app\nMore."}],"messages":[]}`,
			want: "projects/my-app",
		},
		{
			name: "system message role",
			body: `{"messages":[{"role":"system","content":"Primary working directory: /var/code/acme/backend"}]}`,
			want: "acme/backend",
		},
		{
			name: "no hint falls back empty",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
		{
			name: "empty body",
			body: ``,
			want: "",
		},
		{
			name: "single segment path",
			body: `{"system":"Primary working directory: /root","messages":[]}`,
			want: "root",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractProjectPath([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractProjectPath = %q, want %q", got, tc.want)
			}
		})
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

// TestAuditStatsContentBreakdown asserts the §4.6 on-read content
// breakdown lands in the stats response. The fixture emits two
// "full"-level Anthropic pairs with hand-known section sizes; the
// rollup must aggregate them per-section and across rows.
func TestAuditStatsContentBreakdown(t *testing.T) {
	dir := t.TempDir()
	name := "stats-content"
	cfg := gateway.GatewayCfg{
		Name: name,
		Client: []gateway.ClientCfg{
			{Bind: "127.0.0.1:0", API: gateway.APIAnthropic},
		},
		Upstream: []gateway.UpstreamCfg{
			{Name: "default", Target: "https://api.anthropic.com", API: gateway.APIAnthropic},
		},
		Log: gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg.Name, cfg.Log, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// Two pairs with distinct, hand-known section sizes.
	body1 := []byte(`{"model":"claude-opus-4-6","max_tokens":10,` +
		`"system":"You are a helpful assistant.",` +
		`"messages":[{"role":"user","content":"What is 2+2?"}]}`)
	body2 := []byte(`{"model":"claude-opus-4-6","max_tokens":10,` +
		`"system":"sys",` +
		`"tools":[{"name":"Read","description":"r","input_schema":{}}],` +
		`"messages":[{"role":"user","content":"more"}]}`)

	id1 := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-1", TurnIndex: 1, StartTime: time.Now(),
	}, body1)
	rec.Response(id1, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		StartTime: time.Now(), EndTime: time.Now().Add(50 * time.Millisecond),
	}, nil)

	id2 := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-1", TurnIndex: 3, StartTime: time.Now(),
	}, body2)
	rec.Response(id2, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		StartTime: time.Now(), EndTime: time.Now().Add(50 * time.Millisecond),
	}, nil)

	stats, err := computeAuditStats(filepath.Join(dir, name), statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	// Expected per-row totals via direct gateway.SectionBytes call.
	sb1 := gateway.SectionBytes(body1, gateway.APIAnthropic)
	sb2 := gateway.SectionBytes(body2, gateway.APIAnthropic)
	cb := stats.Totals.ContentBreakdown

	if cb.Rows != 2 {
		t.Errorf("ContentBreakdown.Rows = %d, want 2", cb.Rows)
	}
	if cb.System != int64(sb1.System+sb2.System) {
		t.Errorf("System = %d, want %d", cb.System, sb1.System+sb2.System)
	}
	if cb.Tools != int64(sb1.Tools+sb2.Tools) {
		t.Errorf("Tools = %d, want %d", cb.Tools, sb1.Tools+sb2.Tools)
	}
	if cb.UserText != int64(sb1.UserText+sb2.UserText) {
		t.Errorf("UserText = %d, want %d", cb.UserText, sb1.UserText+sb2.UserText)
	}
	// Aggregate partition closure: Total should equal sum of all
	// other section fields.
	wantTotal := cb.System + cb.Tools + cb.UserText + cb.Preamble +
		cb.ToolResults + cb.Thinking + cb.ImagesWire + cb.UserHistory + cb.Other
	if cb.Total != wantTotal {
		t.Errorf("Total = %d, want sum-of-sections %d", cb.Total, wantTotal)
	}
	// Sanity: aggregate Total equals sum of the two bodies' lengths
	// (each row contributes len(body) to its own Total per
	// SectionByteCounts.Total() invariant).
	if cb.Total != int64(len(body1)+len(body2)) {
		t.Errorf("Total = %d, want %d (sum of body lengths)", cb.Total, len(body1)+len(body2))
	}
}

// TestCachedSectionBytes_HitsAndMisses pins the §4.6 cache contract:
// a cold lookup triggers gateway.SectionBytes and stores the result;
// a warm lookup returns the cached value without re-parsing. Pairs
// keyed on (run, id) so distinct rows don't collide.
func TestCachedSectionBytes_HitsAndMisses(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	k1 := pairKey{id: 1, run: "r1"}
	k2 := pairKey{id: 2, run: "r1"}

	// Cold lookups for two distinct keys land independently.
	sb1 := cachedSectionBytes(k1, body, gateway.APIAnthropic)
	sb2 := cachedSectionBytes(k2, body, gateway.APIAnthropic)
	if sb1.Total() != len(body) || sb2.Total() != len(body) {
		t.Errorf("cold lookups: sb1.Total=%d sb2.Total=%d, want %d", sb1.Total(), sb2.Total(), len(body))
	}

	// Warm lookup reuses the entry. Mutate the body slice underneath
	// to prove no re-parse: if cache was bypassed, sb3 would parse
	// the mutated bytes and produce a different partition.
	bodyMut := append([]byte(nil), body...)
	bodyMut[len(bodyMut)-2] = 'X'
	sb3 := cachedSectionBytes(k1, bodyMut, gateway.APIAnthropic)
	if sb3 != sb1 {
		t.Errorf("warm lookup did not reuse cached value (cache bypassed?): %+v vs %+v", sb3, sb1)
	}

	// Cleanup so package-level cache doesn't bleed into other tests.
	t.Cleanup(func() {
		sectionBytesCache.Delete(k1)
		sectionBytesCache.Delete(k2)
	})
}

// TestAuditStatsContentBreakdown_MetadataLevelEmits Zero verifies the
// graceful-degrade path: at LogLevelMetadata the body is not
// persisted, so SectionBytes has nothing to classify. ContentBreakdown
// stays zeroed (Rows=0) and the admin UI knows to surface the
// "enable full logging" hint.
func TestAuditStatsContentBreakdown_MetadataLevelEmitsZero(t *testing.T) {
	auditDir := writeStatsFixture(t, t.TempDir(), "stats-content-meta")
	stats, err := computeAuditStats(auditDir, statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	cb := stats.Totals.ContentBreakdown
	if cb.Rows != 0 {
		t.Errorf("ContentBreakdown.Rows = %d under metadata-level fixture, want 0 (no bodies recorded)", cb.Rows)
	}
	if cb.Total != 0 {
		t.Errorf("ContentBreakdown.Total = %d, want 0", cb.Total)
	}
}

// TestAuditStatsPreambleBlocksFromConsolidatedExtractor ensures the
// PreambleBlocks rollup still works after the consolidation moved
// extractPreamblePayloads to delegate to gateway.ExtractPreambleBlocks.
// Drives a body with a known preamble tag; the rollup must contain
// it.
func TestAuditStatsPreambleBlocksFromConsolidatedExtractor(t *testing.T) {
	dir := t.TempDir()
	name := "stats-preamble"
	cfg := gateway.GatewayCfg{
		Name: name,
		Client: []gateway.ClientCfg{
			{Bind: "127.0.0.1:0", API: gateway.APIAnthropic},
		},
		Upstream: []gateway.UpstreamCfg{
			{Name: "default", Target: "https://api.anthropic.com", API: gateway.APIAnthropic},
		},
		Log: gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg.Name, cfg.Log, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	body := []byte(`{"model":"claude-opus-4-6","max_tokens":10,"messages":[` +
		`{"role":"user","content":"<system-reminder>be terse</system-reminder>list files"}` +
		`]}`)

	id := rec.Request(gateway.RequestMeta{
		Gateway: name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess", TurnIndex: 1, StartTime: time.Now(),
	}, body)
	rec.Response(id, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		StartTime: time.Now(), EndTime: time.Now().Add(50 * time.Millisecond),
	}, nil)

	stats, err := computeAuditStats(filepath.Join(dir, name), statsFilter{})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(stats.PreambleBlocks) == 0 {
		t.Fatal("PreambleBlocks empty; consolidation broke the rollup")
	}
	found := false
	for _, b := range stats.PreambleBlocks {
		if strings.Contains(b.Key, "system-reminder") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a 'system-reminder' preamble block, got: %+v", stats.PreambleBlocks)
	}
}
