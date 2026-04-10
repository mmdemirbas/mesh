//go:build e2e

// Package scenarios contains the end-to-end scenario suite for mesh. Each
// scenario boots one or more mesh-e2e:local containers on a per-test docker
// network and drives real mesh binaries through the harness helpers.
//
// Scenarios are gated behind the "e2e" build tag and invoked by
// `task e2e` (or step 7 of `task check`). They assume `task build:e2e-image`
// has already run; the harness skips with a friendly message otherwise.
package scenarios
