// Package e2e contains the end-to-end container-based test harness for mesh.
//
// The harness builds a Linux binary of mesh, bakes it into an Alpine image,
// and runs scenario tests against real containers via testcontainers-go.
// Scenarios cover SSH tunneling, filesync, clipsync, and the LLM gateway.
//
// All scenario and harness code is gated behind the "e2e" build tag. Churn
// suites use "e2e_churn". Plain `go build ./...` and the release binary never
// link the test-only dependencies.
//
// Entry points:
//
//	task e2e           run the scenario suite
//	task e2e:churn     run the long-running churn suite
//	task e2e:full      run both
//	task e2e:compose:up / down  manual full-topology playground
package e2e
