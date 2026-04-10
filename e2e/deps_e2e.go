//go:build e2e

package e2e

// Blank imports pin the test-only dependencies to the module graph so
// `go mod tidy` keeps them promoted to a direct require. Files under the
// e2e build tag do the real work; this file exists only so the empty
// skeleton still anchors testcontainers-go in the module.
import (
	_ "github.com/testcontainers/testcontainers-go"
)
