package config

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzLoadUnvalidated(f *testing.F) {
	f.Add([]byte(`node1:
  listeners:
    - type: socks
      bind: "127.0.0.1:1080"
`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte("node:\n  log_level: debug\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Skip()
		}
		// Must not panic on arbitrary YAML
		_, _ = LoadUnvalidated(path)
	})
}
