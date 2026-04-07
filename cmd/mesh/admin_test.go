package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	srv := httptest.NewServer(buildAdminMux(ring))
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

	srv := httptest.NewServer(buildAdminMux(ring))
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
	srv := httptest.NewServer(buildAdminMux(ring))
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

func TestAdminMetricsDownComponent(t *testing.T) {
	state.Global.Update("server", "admintest-down:9999", state.Failed, "refused")
	t.Cleanup(func() {
		state.Global.Delete("server", "admintest-down:9999")
	})

	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring))
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
	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring))
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

func TestAdminRootRedirect(t *testing.T) {
	ring := newLogRing(4)
	srv := httptest.NewServer(buildAdminMux(ring))
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
	ring := newLogRing(8)
	srv := httptest.NewServer(buildAdminMux(ring))
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
	ring := newLogRing(4)
	mux := buildAdminMux(ring)
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
