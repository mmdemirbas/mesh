//go:build e2e

package harness

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"text/template"
)

// FixturesDir resolves the e2e/fixtures directory relative to this source
// file. Using runtime.Caller lets tests run from any working directory —
// `go test ./e2e/scenarios/...` sets the CWD to the scenario package, not
// the module root.
func FixturesDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "fixtures"
	}
	return filepath.Join(filepath.Dir(file), "..", "fixtures")
}

// LoadTemplate reads a named fixture file from e2e/fixtures/ and returns
// its contents. Paths are resolved relative to FixturesDir so scenarios
// can pass short names like "configs/s1-bastion.yaml".
func LoadTemplate(name string) (string, error) {
	path := filepath.Join(FixturesDir(), name)
	b, err := os.ReadFile(path) //nolint:gosec // G304: path constrained to fixtures dir by the caller.
	if err != nil {
		return "", fmt.Errorf("read fixture %s: %w", name, err)
	}
	return string(b), nil
}

// RenderTemplate loads a fixture, runs it through text/template with the
// provided data, and returns the rendered YAML. Scenarios use this to
// inject per-test values (aliases, ports, key paths) into a shared config
// template without sprinkling Sprintf calls through the test body.
func RenderTemplate(name string, data any) (string, error) {
	body, err := LoadTemplate(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parse fixture %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render fixture %s: %w", name, err)
	}
	return buf.String(), nil
}
