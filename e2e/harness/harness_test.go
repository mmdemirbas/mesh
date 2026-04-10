//go:build e2e

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetricValue(t *testing.T) {
	body := `# HELP mesh_bytes_tx_total Total bytes transmitted per component.
# TYPE mesh_bytes_tx_total counter
mesh_bytes_tx_total{type="forward",id="2222"} 12345
mesh_bytes_tx_total{type="listener",id="sshd"} 6789
# HELP mesh_component_up Whether the component is up (1) or down (0).
# TYPE mesh_component_up gauge
mesh_component_up{type="forward",id="2222",status="listening"} 1
`

	tests := []struct {
		name   string
		metric string
		sel    string
		want   float64
		ok     bool
	}{
		{"tx for forward", "mesh_bytes_tx_total", `id="2222"`, 12345, true},
		{"tx for listener", "mesh_bytes_tx_total", `id="sshd"`, 6789, true},
		{"gauge up", "mesh_component_up", `id="2222"`, 1, true},
		{"missing metric", "mesh_other", "", 0, false},
		{"missing selector", "mesh_bytes_tx_total", `id="nope"`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MetricValue(body, tt.metric, tt.sel)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("MetricValue(%q, %q) = (%v, %v) want (%v, %v)", tt.metric, tt.sel, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRenderTemplate(t *testing.T) {
	dir := t.TempDir()
	fixDir := filepath.Join(dir, "fixtures")
	if err := os.MkdirAll(fixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixDir, "hello.tmpl"), []byte("hello {{.Name}}!"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relocate the package fixtures dir for this test by temporarily
	// copying into the real location.
	realDir := FixturesDir()
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(realDir, "harness-test-hello.tmpl")
	if err := os.WriteFile(tmp, []byte("hello {{.Name}}!"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tmp) })

	out, err := RenderTemplate("harness-test-hello.tmpl", map[string]string{"Name": "mesh"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "hello mesh!" {
		t.Fatalf("got %q want %q", out, "hello mesh!")
	}

	if _, err := RenderTemplate("does-not-exist.tmpl", nil); err == nil {
		t.Fatal("expected missing fixture error")
	}

	if _, err := RenderTemplate("harness-test-hello.tmpl", map[string]string{}); err == nil {
		t.Fatal("expected missingkey error for empty data")
	}
}
