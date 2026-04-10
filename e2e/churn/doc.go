//go:build e2e_churn

// Package churn contains heavy end-to-end stress tests for mesh.
//
// Scenarios here are gated behind the "e2e_churn" build tag so neither
// go build ./..., the default unit lane, nor the regular e2e lane ever
// picks them up. They are invoked via `task e2e:churn` (which feeds
// -tags e2e_churn into `go test ./e2e/churn/...`) and run on a per-test
// budget of ~2 minutes so the full suite fits in ~10 minutes.
//
// The churn suite is nightly-grade: the goal is to surface scale bugs
// and fsnotify races, not to block release on every build.
package churn
