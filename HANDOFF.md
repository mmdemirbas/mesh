# Handoff

## Status
WAITING_FOR_REVIEW

## Round
1

## From → To
coder → reviewer

---

## Message

Added `t.Parallel()` to test functions and subtests across all packages. 343 parallel markers added total.

**Exclusions applied:**
- `TestSSHServerExec` and `TestSSHServerLocalForward` in `tunnel_test.go` — both poll `state.Global` for server readiness.
- `TestAdminStateEndpoints`, `TestAdminLogsEndpoint`, `TestAdminMetricsEndpoint`, `TestAdminMetricsDownComponent`, `TestAdminHealthz` in `cmd/mesh/admin_test.go` — use `state.Global` via `adminTestSetup`.
- `TestPortFileWriteAndCleanup` in `cmd/mesh/admin_test.go` — writes to a fixed path (`~/.mesh/run/mesh-test-cleanup.port`).
- `TestLoadUnvalidated_EnvExpansion` in `config_test.go` — uses `t.Setenv`.
- `TestBuildAuthMethods_AgentWithoutSocket` in `tunnel_test.go` — uses `t.Setenv`.
- `TestGateway_O2A_NonStreaming` in `gateway_test.go` — uses `t.Setenv`.

All subtests in table-driven tests were also parallelized where safe. Verified with `go test -count=1 -race ./...` — all packages pass.

## Files Changed
- `internal/state/state_test.go` — added `t.Parallel()` to all 24 test functions
- `internal/config/config_test.go` — added `t.Parallel()` to all eligible tests and subtests; excluded `TestLoadUnvalidated_EnvExpansion`
- `internal/filesync/filesync_test.go` — added `t.Parallel()` to all tests and subtests
- `internal/clipsync/clipsync_test.go` — added `t.Parallel()` to all tests and subtests
- `internal/gateway/a2o_test.go` — added `t.Parallel()` to all tests
- `internal/gateway/config_test.go` — added `t.Parallel()` to all tests and subtests
- `internal/gateway/gateway_test.go` — added `t.Parallel()` to eligible tests; excluded `TestGateway_O2A_NonStreaming`
- `internal/gateway/o2a_test.go` — added `t.Parallel()` to all tests
- `internal/gateway/stream_test.go` — added `t.Parallel()` to all tests
- `internal/tunnel/tunnel_test.go` — added `t.Parallel()` to eligible tests; excluded `TestSSHServerExec`, `TestSSHServerLocalForward`, `TestBuildAuthMethods_AgentWithoutSocket`
- `internal/tunnel/env_test.go` — added `t.Parallel()` to all tests and subtests
- `internal/tunnel/signal_test.go` — added `t.Parallel()` to all tests and subtests
- `internal/proxy/proxy_test.go` — added `t.Parallel()` to all tests
- `internal/netutil/netutil_test.go` — added `t.Parallel()` to all tests
- `cmd/mesh/admin_test.go` — added `t.Parallel()` to `TestAdminUIEndpoint`, `TestAdminRootRedirect`, `TestAdminLogsEmpty`, `TestAdminServerRandomPortBind`, `TestPortFilePath`

## Action Items
