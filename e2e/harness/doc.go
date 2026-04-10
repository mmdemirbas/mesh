//go:build e2e

// Package harness provides the primitives scenarios use to stand up mesh
// containers on isolated docker networks, drive them, and collect artefacts
// when a test fails.
//
// Scenarios build a Network, start one or more Nodes on it with a YAML
// config, and then drive the nodes via Exec, WriteFile, ReadFile, and the
// AdminAPI helpers. On test failure, artifacts.go dumps container logs,
// mesh logs, /api/state, /api/metrics, and a short packet capture to a
// per-test directory so the failure can be diagnosed after the fact.
//
// Everything in this package is gated behind the "e2e" build tag.
package harness
