package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/gateway"
	"github.com/mmdemirbas/mesh/internal/state"
)

// adminTestSetup registers a component in state.Global and returns a cleanup function.
// Tests must call t.Cleanup with the returned function to avoid polluting other tests.
func adminTestSetup(t *testing.T) *logRing {
	t.Helper()
	state.Global.Update("server", "admintest:22", state.Connected, "peer-10.0.0.1")
	m := state.Global.GetMetrics("server", "admintest:22")
	m.BytesTx.Store(100)
	m.BytesRx.Store(200)
	m.Streams.Store(3)
	m.StartTime.Store(1_000_000_000) // 1 second in nanoseconds
	t.Cleanup(func() {
		state.Global.Delete("server", "admintest:22")
		state.Global.DeleteMetrics("server", "admintest:22")
	})
	ring := newLogRing(16)
	return ring
}

func TestAdminStateEndpoints(t *testing.T) {
	ring := adminTestSetup(t)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	for _, path := range []string{"/api/state"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			body, _ := io.ReadAll(resp.Body)

			var snap map[string]state.Component
			if err := json.Unmarshal(body, &snap); err != nil {
				t.Fatalf("invalid JSON: %v\nbody: %s", err, body)
			}
			comp, ok := snap["server:admintest:22"]
			if !ok {
				t.Fatalf("component server:admintest:22 not found in snapshot; keys: %v", keys(snap))
			}
			if comp.Status != state.Connected {
				t.Errorf("status = %q, want %q", comp.Status, state.Connected)
			}
		})
	}
}

func TestAdminLogsEndpoint(t *testing.T) {
	ring := adminTestSetup(t)
	// Write a line with ANSI escape codes and a plain line.
	ring.Write([]byte("\x1b[32mgreen text\x1b[0m\n"))
	ring.Write([]byte("plain line\n"))

	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/logs")
	if err != nil {
		t.Fatalf("GET /api/logs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var lines []string
	if err := json.Unmarshal(body, &lines); err != nil {
		t.Fatalf("invalid JSON: %v\nbody: %s", err, body)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	// ANSI codes must be stripped.
	if strings.ContainsRune(lines[0], '\x1b') {
		t.Errorf("line[0] still contains ANSI escape: %q", lines[0])
	}
	if lines[0] != "green text" {
		t.Errorf("line[0] = %q, want %q", lines[0], "green text")
	}
	if lines[1] != "plain line" {
		t.Errorf("line[1] = %q, want %q", lines[1], "plain line")
	}
}

func TestAdminMetricsEndpoint(t *testing.T) {
	ring := adminTestSetup(t)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	checks := []struct {
		desc    string
		contain string
	}{
		{"component_up HELP", "# HELP mesh_component_up"},
		{"component_up TYPE gauge", "# TYPE mesh_component_up gauge"},
		{"component_up value 1 for connected", `mesh_component_up{type="server",id="admintest:22",status="connected"} 1`},
		{"bytes_tx HELP", "# HELP mesh_bytes_tx_total"},
		{"bytes_tx TYPE counter", "# TYPE mesh_bytes_tx_total counter"},
		{"bytes_tx value", `mesh_bytes_tx_total{type="server",id="admintest:22"} 100`},
		{"bytes_rx value", `mesh_bytes_rx_total{type="server",id="admintest:22"} 200`},
		{"active_streams TYPE gauge", "# TYPE mesh_active_streams gauge"},
		{"active_streams value", `mesh_active_streams{type="server",id="admintest:22"} 3`},
		{"uptime TYPE gauge", "# TYPE mesh_uptime_seconds gauge"},
		{"auth_failures TYPE counter", "# TYPE mesh_auth_failures_total counter"},
		{"goroutines HELP", "# HELP mesh_process_goroutines"},
		{"goroutines TYPE gauge", "# TYPE mesh_process_goroutines gauge"},
		{"goroutines value", "mesh_process_goroutines "},
		{"state_components HELP", "# HELP mesh_state_components"},
		{"state_components TYPE gauge", "# TYPE mesh_state_components gauge"},
		{"state_components value", "mesh_state_components "},
		{"state_metrics HELP", "# HELP mesh_state_metrics"},
		{"state_metrics TYPE gauge", "# TYPE mesh_state_metrics gauge"},
		{"state_metrics value", "mesh_state_metrics "},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.contain) {
			t.Errorf("%s: output does not contain %q", c.desc, c.contain)
		}
	}

	// Verify uptime line is present for the test component (StartTime != 0).
	if !strings.Contains(text, `mesh_uptime_seconds{type="server",id="admintest:22"}`) {
		t.Error("uptime line missing for test component with non-zero StartTime")
	}
}

func TestAdminGatewayAuditEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := gateway.GatewayCfg{
		Name:        "audit-endpoint-test",
		Bind:        "127.0.0.1:0",
		Upstream:    "https://api.anthropic.com",
		ClientAPI:   gateway.APIAnthropic,
		UpstreamAPI: gateway.APIAnthropic,
		Log:         gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	id := rec.Request(gateway.RequestMeta{
		Gateway: "audit-endpoint-test", Direction: "a2a",
		Model: "claude-opus-4-6", Stream: false,
		Method: "POST", Path: "/v1/messages",
		StartTime: time.Now(),
	}, []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`))
	rec.Response(id, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		Usage:     &gateway.Usage{InputTokens: 5, OutputTokens: 11},
		StartTime: time.Now(), EndTime: time.Now(),
	}, []byte(`{"content":[{"type":"text","text":"hi back"}]}`))

	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gateway/audit")
	if err != nil {
		t.Fatalf("GET /api/gateway/audit: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	var got []struct {
		Gateway string            `json:"gateway"`
		Dir     string            `json:"dir"`
		File    string            `json:"file"`
		Rows    []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse response: %v body=%s", err, body)
	}

	var entry *struct {
		Gateway string            `json:"gateway"`
		Dir     string            `json:"dir"`
		File    string            `json:"file"`
		Rows    []json.RawMessage `json:"rows"`
	}
	for i := range got {
		if got[i].Gateway == "audit-endpoint-test" {
			entry = &got[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("audit-endpoint-test not found in response: %s", body)
	}
	if !strings.HasSuffix(entry.File, ".jsonl") {
		t.Errorf("file = %q, want *.jsonl", entry.File)
	}
	if len(entry.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(entry.Rows))
	}
	var first map[string]any
	if err := json.Unmarshal(entry.Rows[0], &first); err != nil {
		t.Fatalf("row[0] not JSON: %v", err)
	}
	if first["t"] != "req" {
		t.Errorf("row[0].t = %v, want req", first["t"])
	}
	if first["model"] != "claude-opus-4-6" {
		t.Errorf("row[0].model = %v", first["model"])
	}
}

// TestAdminGatewayAuditFilters drives the new query parameters end-to-end:
// session, model, outcome, min_tokens. Two pairs are written with distinct
// sessions/models, then each filter must isolate the right one.
func TestAdminGatewayAuditFilters(t *testing.T) {
	dir := t.TempDir()
	cfg := gateway.GatewayCfg{
		Name:        "audit-filter-test",
		Bind:        "127.0.0.1:0",
		Upstream:    "https://api.anthropic.com",
		ClientAPI:   gateway.APIAnthropic,
		UpstreamAPI: gateway.APIAnthropic,
		Log:         gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// Pair A: opus, session "sess-a", high tokens, OK.
	idA := rec.Request(gateway.RequestMeta{
		Gateway: cfg.Name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-a", TurnIndex: 1,
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec.Response(idA, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		Usage:     &gateway.Usage{InputTokens: 500, OutputTokens: 500},
		StartTime: time.Now(), EndTime: time.Now(),
	}, []byte(`{}`))

	// Pair B: sonnet, session "sess-b", low tokens, error.
	idB := rec.Request(gateway.RequestMeta{
		Gateway: cfg.Name, Direction: "a2a",
		Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages",
		SessionID: "sess-b", TurnIndex: 1,
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"yo"}]}`))
	rec.Response(idB, gateway.ResponseMeta{
		Status: 500, Outcome: gateway.OutcomeError,
		Usage:     &gateway.Usage{InputTokens: 5, OutputTokens: 5},
		StartTime: time.Now(), EndTime: time.Now(),
	}, []byte(`{}`))

	srv := httptest.NewServer(buildAdminMux(newLogRing(4), ""))
	defer srv.Close()

	type entry struct {
		Gateway string            `json:"gateway"`
		Rows    []json.RawMessage `json:"rows"`
	}
	getRows := func(query string) []json.RawMessage {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/gateway/audit?gateway=" + cfg.Name + "&" + query)
		if err != nil {
			t.Fatalf("GET %s: %v", query, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got []entry
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 entry for %s, got %d", query, len(got))
		}
		return got[0].Rows
	}
	requireSession := func(rows []json.RawMessage, want string) {
		t.Helper()
		for _, r := range rows {
			var m map[string]any
			_ = json.Unmarshal(r, &m)
			if m["t"] != "req" {
				continue
			}
			if got, _ := m["session_id"].(string); got != want {
				t.Errorf("row session_id=%q, want %q", got, want)
			}
		}
	}

	// session filter.
	rowsA := getRows("session=sess-a")
	if len(rowsA) != 2 {
		t.Errorf("session=sess-a returned %d rows, want 2", len(rowsA))
	}
	requireSession(rowsA, "sess-a")

	// model filter.
	rowsSonnet := getRows("model=claude-sonnet-4-6")
	if len(rowsSonnet) != 2 {
		t.Errorf("model filter returned %d rows, want 2", len(rowsSonnet))
	}
	requireSession(rowsSonnet, "sess-b")

	// outcome filter.
	rowsErr := getRows("outcome=error")
	if len(rowsErr) != 2 {
		t.Errorf("outcome=error returned %d rows, want 2", len(rowsErr))
	}
	requireSession(rowsErr, "sess-b")

	// min_tokens filter (pair A has 1000 total, B has 10).
	rowsBig := getRows("min_tokens=100")
	if len(rowsBig) != 2 {
		t.Errorf("min_tokens=100 returned %d rows, want 2", len(rowsBig))
	}
	requireSession(rowsBig, "sess-a")
}

// TestAdminGatewayAuditPair verifies the /pair endpoint returns full request
// and response rows for a known id+run.
func TestAdminGatewayAuditPair(t *testing.T) {
	dir := t.TempDir()
	cfg := gateway.GatewayCfg{
		Name:        "audit-pair-test",
		Bind:        "127.0.0.1:0",
		Upstream:    "https://api.anthropic.com",
		ClientAPI:   gateway.APIAnthropic,
		UpstreamAPI: gateway.APIAnthropic,
		Log:         gateway.LogCfg{Level: gateway.LogLevelFull, Dir: dir, MaxFileSize: "10MB", MaxAge: "720h"},
	}
	rec, err := gateway.NewRecorder(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || rec == nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	id := rec.Request(gateway.RequestMeta{
		Gateway: cfg.Name, Direction: "a2a",
		Model: "claude-opus-4-6", Method: "POST", Path: "/v1/messages",
		StartTime: time.Now(),
	}, []byte(`{"messages":[{"role":"user","content":"only-pair"}]}`))
	rec.Response(id, gateway.ResponseMeta{
		Status: 200, Outcome: gateway.OutcomeOK,
		Usage:     &gateway.Usage{InputTokens: 1, OutputTokens: 2},
		StartTime: time.Now(), EndTime: time.Now(),
	}, []byte(`{"content":[{"type":"text","text":"ack"}]}`))

	// Discover the run id via the list endpoint so the test is independent of
	// internal id-generation choices.
	srv := httptest.NewServer(buildAdminMux(newLogRing(4), ""))
	defer srv.Close()

	listResp, err := http.Get(srv.URL + "/api/gateway/audit?gateway=" + cfg.Name)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []struct {
		Rows []json.RawMessage `json:"rows"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	_ = listResp.Body.Close()
	if len(list) != 1 || len(list[0].Rows) == 0 {
		t.Fatalf("list returned no rows: %+v", list)
	}
	var first map[string]any
	_ = json.Unmarshal(list[0].Rows[0], &first)
	run, _ := first["run"].(string)
	if run == "" {
		t.Fatalf("no run id in row: %v", first)
	}

	pairURL := fmt.Sprintf("%s/api/gateway/audit/pair?gateway=%s&id=%d&run=%s",
		srv.URL, cfg.Name, uint64(id), run)
	pairResp, err := http.Get(pairURL)
	if err != nil {
		t.Fatalf("pair GET: %v", err)
	}
	defer func() { _ = pairResp.Body.Close() }()
	if pairResp.StatusCode != 200 {
		body, _ := io.ReadAll(pairResp.Body)
		t.Fatalf("pair status=%d body=%s", pairResp.StatusCode, body)
	}
	var pair map[string]json.RawMessage
	if err := json.NewDecoder(pairResp.Body).Decode(&pair); err != nil {
		t.Fatalf("decode pair: %v", err)
	}
	if len(pair["request"]) == 0 || len(pair["response"]) == 0 {
		t.Fatalf("pair missing halves: %+v", pair)
	}

	// Unknown pair → 404.
	miss, err := http.Get(srv.URL + "/api/gateway/audit/pair?gateway=" + cfg.Name + "&id=999999&run=" + run)
	if err != nil {
		t.Fatalf("miss GET: %v", err)
	}
	_ = miss.Body.Close()
	if miss.StatusCode != 404 {
		t.Errorf("missing pair status = %d, want 404", miss.StatusCode)
	}
}

func TestAdminMetricsGatewayTokens(t *testing.T) {
	state.Global.Update("gateway", "metrics-gw-test", state.Listening, "127.0.0.1:9999")
	m := state.Global.GetMetrics("gateway", "metrics-gw-test")
	m.TokensIn.Store(1234)
	m.TokensOut.Store(5678)
	t.Cleanup(func() {
		state.Global.Delete("gateway", "metrics-gw-test")
		state.Global.DeleteMetrics("gateway", "metrics-gw-test")
	})

	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		"# TYPE mesh_gateway_tokens_in_total counter",
		"# TYPE mesh_gateway_tokens_out_total counter",
		`mesh_gateway_tokens_in_total{id="metrics-gw-test"} 1234`,
		`mesh_gateway_tokens_out_total{id="metrics-gw-test"} 5678`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestAdminMetricsDownComponent(t *testing.T) {
	state.Global.Update("server", "admintest-down:9999", state.Failed, "refused")
	t.Cleanup(func() {
		state.Global.Delete("server", "admintest-down:9999")
	})

	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, `mesh_component_up{type="server",id="admintest-down:9999",status="failed"} 0`) {
		t.Errorf("expected up=0 for failed component; output:\n%s", text)
	}
}

func TestAdminUIEndpoint(t *testing.T) {
	t.Parallel()
	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	for _, path := range []string{"/ui", "/ui/filesync", "/ui/logs", "/ui/api"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "<!DOCTYPE html>") {
				t.Error("response body does not contain <!DOCTYPE html>")
			}
		})
	}
}

// TestAdminUIGatewayDetailMarkup pins the request/response split layout and
// the JSON syntax-highlighter style hooks so a future cleanup pass does not
// silently revert the detail card to a single <pre> block.
func TestAdminUIGatewayDetailMarkup(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(buildAdminMux(newLogRing(4), ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ui/gateway")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{
		`id="gw-req-raw"`,
		`id="gw-resp-raw"`,
		`id="gw-req-structured"`,
		`id="gw-resp-structured"`,
		`gw-detail-grid`,
		`function highlightJSON`,
		`json-key`,
		`json-str`,
		`copyDetail`,
		`renderRequestStructured`,
		`renderResponseStructured`,
		`renderTokenBar`,
		`token-bar`,
		`seg-cache-read`,
		`msg-role`,
		`tool-block`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("UI missing %q (regression: detail layout collapsed back to single pane)", want)
		}
	}
}

func TestAdminRootRedirect(t *testing.T) {
	t.Parallel()
	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc := resp.Header.Get("Location")
	if loc != "/ui" {
		t.Errorf("Location = %q, want /ui", loc)
	}
}

func TestAdminLogsEmpty(t *testing.T) {
	t.Parallel()
	ring := newLogRing(8)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/logs")
	if err != nil {
		t.Fatalf("GET /api/logs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var lines []string
	if err := json.Unmarshal(body, &lines); err != nil {
		t.Fatalf("invalid JSON for empty ring: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected empty array, got %v", lines)
	}
}

func TestAdminServerRandomPortBind(t *testing.T) {
	t.Parallel()
	ring := newLogRing(4)
	mux := buildAdminMux(ring, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	if port == 0 {
		t.Fatal("expected a non-zero port from random binding")
	}

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", port))
	if err != nil {
		t.Fatalf("admin server not reachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPortFilePath(t *testing.T) {
	t.Parallel()
	path := portFilePath("testnode")
	if path == "" {
		t.Fatal("portFilePath returned empty")
	}
	if !strings.HasSuffix(path, "mesh-testnode.port") {
		t.Errorf("unexpected path suffix: %s", path)
	}
	if !strings.Contains(path, filepath.Join(".mesh", "run")) {
		t.Errorf("path does not contain .mesh/run directory: %s", path)
	}
}

func TestPortFileWriteAndCleanup(t *testing.T) {
	path := portFilePath("test-cleanup")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "12345" {
		t.Errorf("port file content = %q, want %q", data, "12345")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("port file should be removed, but stat returned: %v", err)
	}
}

// keys returns the map keys as a slice for error messages.
func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAdminHealthz(t *testing.T) {
	ring := adminTestSetup(t)
	srv := httptest.NewServer(buildAdminMux(ring, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}
