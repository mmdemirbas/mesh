# mesh — Architecture & Conventions

> For AI assistants and new contributors. Read this before making changes.
>
> **Local environment**: See `CLAUDE.local.md` (git-excluded) for machine-specific
> paths, log locations, and debugging checklist. Always check it before diagnosing
> production issues.

## What is mesh?

A single-binary, cross-platform networking tool that replaces `ssh`, `sshd`, `autossh`, `socat`, SOCKS/HTTP proxy servers, and file sync tools. One config file defines listeners, outbound connections, port forwards, clipboard sync, and folder synchronization.

## Project Structure

```
cmd/mesh/           CLI entry point, dashboard, status rendering
internal/
  config/           YAML config loading, validation, structs
  tunnel/           SSH client + server, port forwarding, keepalives
  proxy/            SOCKS5 + HTTP CONNECT proxy servers
  netutil/          TCP helpers (BiCopy, keepalive, reusable listeners)
  clipsync/         Clipboard sync (UDP discovery, protobuf HTTP push/pull, OS clipboard I/O)
  clipsync/proto/   Protobuf definitions for clipsync (SyncPayload, Beacon, DiscoverRequest)
  filesync/         Folder sync (protobuf index exchange, delta sync, fsnotify)
  filesync/proto/   Protobuf definitions for filesync (IndexExchange, BlockSignatures, DeltaResponse)
  gateway/          LLM API gateway (Anthropic <-> OpenAI format translation)
  state/            Thread-safe component state (Global singleton with Snapshot())
configs/            Example YAML, production config (mesh.yaml), JSON schema
e2e/                End-to-end scenario suite — real containers, never linked into the release binary.
  harness/          testcontainers-go helpers: Network, Node, Eventually, WaitForComponent, KeyPair, artifact dump. Build tag: e2e || e2e_churn.
  scenarios/        S1-S4 scenarios (SSH bastion, filesync, clipsync, gateway). Build tag: e2e.
  churn/            Heavy stress tests (10k-file propagation, rename storm, concurrent edits). Build tag: e2e_churn. Not in `task check`.
  stub/             In-repo canned-response HTTP server used as the gateway upstream. Build tag: e2e.
  fixtures/         Fake xclip shim, per-scenario YAML configs. Never tagged so go build ./... still parses the module.
  compose/          Manual full-topology playground (docker-compose.yaml + configs + playground keys + README).
  Dockerfile.e2e    Alpine image that bakes the mesh binary, the stub-llm binary, and the fake xclip shim.
```

## Key Design Decisions

**Single `state.Global`** — All components (listeners, connections, forwards, clipsync) update a shared `state.State` via `Update/Delete`. The dashboard and status commands read via `Snapshot()` which returns a copy. Mutex-protected. Each component tracks `LastUpdated`; a background goroutine (`StartEviction`, 5-min interval) removes entries not updated within 1 hour and their orphaned metrics.

**ForwardSet = independent SSH connection** — Each `ForwardSet` within a `Connection` gets its own physical SSH connection for throughput isolation. This is intentional, not a bug.

**Multiplex mode** — `mode: multiplex` on a Connection connects to ALL targets simultaneously (one SSH connection per target). Default `failover` mode tries targets in order.

**Auth method order** — `buildAuthMethods()` tries: SSH agent → private key → password_command. Multiple methods can be configured; they're all offered to the server. Authorized keys are pre-marshaled at load time to avoid repeated marshaling on every auth attempt.

**SSH server features** — `accept_env` config field controls which client environment variables are accepted (supports trailing wildcards like `LC_*`). `banner` and `motd` config fields point to text files read at startup: banner is shown pre-auth via `ssh.ServerConfig.BannerCallback`, MOTD is written to the session channel post-auth before shell I/O starts. Signal forwarding per RFC 4254 section 6.9 — `signal` channel requests are parsed and delivered to the process group on Unix via `syscall.Kill(-pid, sig)`, or handled as hard kill on Windows for KILL/TERM/INT/HUP. Exit-signal reporting per RFC 4254 section 6.10 — processes killed by a signal send `exit-signal` with the mapped signal name instead of `exit-status` with code 0. Signal name mapping (`signalName`, `sshSignalNumber`) in `shell_unix.go`. Windows default shell prefers `pwsh.exe` (modern PowerShell) if installed, falls back to `COMSPEC`/`cmd.exe`. Exec commands use `-Command` for PowerShell, `/C` for cmd.exe. Windows shell processes run via fresh goroutine to avoid Go runtime GC stack corruption during `syscall.StartProcess`, with deep-copied environment slices and explicit `cmd.Cancel`/`cmd.WaitDelay` for process cleanup on disconnect.

**Clipsync protocol** — Push-based via HTTP POST with gzip-compressed protobuf serialization (`SyncPayload`, `Beacon`, `DiscoverRequest` in `clipsync/proto/`). `Content-Encoding: gzip` header signals compression on both requests and responses. The sender embeds file data directly in the payload for one-way connectivity (receiver can't pull back). Pull-back via `/files/` endpoint is a fallback for when the receiver CAN reach the sender. UDP discovery uses broadcast beacons (10s interval) with unicast reply for asymmetric networks. Group isolation via `lan_discovery_group` config field (list of strings) — peers with no overlapping group ignore each other; empty list disables dynamic discovery entirely. Clipboard polling interval is configurable via `poll_interval` (default 3s). Local clipboard reads are capped at `maxClipboardPayload` (100 MB total across all formats) to prevent OOM.

**Dashboard** — Uses terminal alternate screen buffer (`\033[?1049h`). Overwrites in-place line by line (`\033[K` per line, `\033[J` to clear remainder). Header (uptime/clock) always written; body (status + logs) skipped when unchanged. No scrollback pollution, no flicker.

**Config precedence** — Hardcoded defaults → config file (YAML) → environment variables (`os.ExpandEnv` in config loading) → CLI flags. Validation at load time with actionable errors.

**Filesync** — Folder sync with named peer definitions and config-level defaults. Peers are declared as a `peers:` map (name → addresses) at the filesync level; folders reference peer names. A `defaults:` section provides fallback values (peers, direction, ignore patterns) that individual folders can override (peers, direction) or extend (ignore patterns are appended). Config is resolved at load time: `FilesyncCfg.Resolve()` merges defaults, resolves peer names to addresses, and populates `ResolvedFolders []FolderCfg`. Runtime code reads only `ResolvedFolders`. Gzip-compressed protobuf index exchange with delta mode (`since` field skips unchanged entries); `Content-Encoding: gzip` header on both requests and responses. Outgoing index is also delta: `PeerState.LastSentSequence` tracks what was already sent to each peer, so subsequent syncs transmit only new entries. Block-level delta transfer via `POST /delta` — receiver sends SHA-256 block signatures (128 KB blocks), sender returns only changed blocks. Bandwidth throttling via `max_bandwidth` config (token-bucket rate limiter). Dual change detection: fsnotify for real-time + periodic scan as safety net. Direction modes: `send-receive` (default), `send-only`, `receive-only`, `dry-run` (scan + compare without file changes), `disabled` (no activity, visible in dashboard). Conflict resolution uses `.sync-conflict-*` naming. Transfer resume via `.mesh-tmp-*` temp files with offset-based HTTP GET. Index persisted as YAML in `~/.mesh/filesync/<folder-id>/`. Ignore patterns are gitignore-style, configured in YAML only (no `.stignore` files). Web UI at `/ui/filesync` on the admin port.

**LLM API Gateway** — Bidirectional translation between Anthropic Messages API and OpenAI Chat Completions API. Two modes: `anthropic-to-openai` (Direction A: Claude Code against non-Anthropic backends like OneAPI, Ollama, vLLM) and `openai-to-anthropic` (Direction B: OpenAI-only tools like Cursor/Aider against Claude). Each gateway instance is an HTTP server on a loopback port. `model_map` remaps model names; `api_key_env` references an env var for the upstream key. Supports streaming (SSE) with per-event translation, tool use (including parallel tool calls), image content (base64 and URL), and error translation (Anthropic 529 ↔ OpenAI 503). Non-loopback bind addresses are rejected at config validation. Request bodies capped at 32 MB, upstream responses at 64 MB, SSE lines at 4 MB. Client-supplied `Authorization`/`x-api-key` headers are ignored; the gateway uses its own configured key. Client model name is echoed back unchanged in responses (no reverse mapping). Tool call IDs passed through unchanged.

**Admin server** — Every `mesh up` starts a local HTTP server on `127.0.0.1:7777`. Port written to `~/.mesh/run/mesh-<node>.port`. UI is a unified SPA at `/ui` with tabs: Dashboard, Clipsync, Filesync, Logs, Metrics, API, Debug. Endpoints: `GET /` (redirects to `/ui`), `GET /ui[/clipsync|/filesync|/logs|/metrics|/api|/debug]` (SPA tabs), `GET /api/state` (JSON state), `GET /api/logs` (recent 1000 log lines from ring buffer), `GET /api/logs/file` (full log file with `?offset=` and `?limit=` params, `X-Log-Size` response header), `GET /api/metrics` (Prometheus text), `GET /api/filesync/folders`, `GET /api/filesync/conflicts`, `GET /api/clipsync/activity`. Configure with `admin_addr` in node config; set to `"off"` to disable. Auth failures are tracked in `tunnel.authFailuresByIP` and exposed via `tunnel.SnapshotAuthFailures()` for the metrics endpoint. Process-level metrics: `mesh_process_goroutines`, `mesh_process_open_fds` (Unix only), `mesh_state_components`, `mesh_state_metrics`. A background self-monitor (`selfmon.go`, 30s interval) logs warnings when goroutines, FDs, or state map sizes exceed 10,000.

## CLI

```
mesh [-f config.yaml] <command> [node...] [flags]
```

When no node names are given, all nodes in the config file are used.
Multiple nodes can be specified and will run within a single process.

| Command | Purpose |
|---|---|
| `up [node...]` | Start nodes with live dashboard (alternate screen when TTY) |
| `down [node...]` | Stop running nodes (SIGTERM + wait) |
| `status [node...] [-w]` | Show node status; `-w` for watch mode |
| `config [node...]` | Show parsed config without starting |
| `completion` | Generate shell completions (bash, zsh, fish) |
| `help` | Detailed help with SSH options |

Shell completions dynamically resolve node names from the config file.

## Conventions

- **ANSI colors** — Package-level vars (`cReset`, `cBold`, etc.), disabled when `NO_COLOR` is set.
- **Address sorting** — `compareAddr()` with `addrKey` pre-parsed into `uint64` pairs for zero-allocation IPv4 comparison.
- **Log handler chain** — `humanLogHandler` wraps tint with package-level key classification maps (zero per-record allocations). Dashboard mode: `humanLogHandler → multiHandler → {tint(file), tint(ring)}`. Non-TTY mode: `humanLogHandler → multiHandler → {tint(stderr), tint(ring)}`. The ring (1000 lines) is always populated regardless of mode so `/api/logs` has data.
- **Platform code** — Build-tagged files (`_unix.go`, `_windows.go`) preferred over `runtime.GOOS` switches. Remaining `runtime.GOOS` uses in clipsync clipboard I/O and CLI pid management are being migrated to build tags.
- **Config validation** — `validate()` checks at load time. Auth requires at least one method. `known_hosts` or `StrictHostKeyChecking: no` required for SSH.
- **Tests** — Table-driven. Real TCP/HTTP for integration tests (`httptest.NewServer`). Fuzz tests for parsers (`go test -fuzz`). No `time.Sleep` — use channels/atomic. Race detector always on.
- **Error handling** — Wrap errors with context: `fmt.Errorf("connections[%d] %q: %w", i, name, err)`. Return errors from libraries; never `log.Fatal` outside `main()`.
- **Resource cleanup** — `defer` for connections, file handles, goroutines. Every goroutine has a clear exit path tied to context cancellation or channel close.
- **Channel close safety** — Use `sync.Once`-guarded close functions when multiple goroutines may close the same channel/connection (see `shell_windows.go`, `shell_unix.go`). For listeners closed from both `defer` and `context.AfterFunc`/goroutines, use `onceCloseListener` wrapper (tunnel.go) or inline `sync.Once` closures (proxy.go, tunnel.go forward handlers).

## Common Patterns

```go
// State update lifecycle
state.Global.Update("connection", id, state.Connecting, "")
// ... connect ...
state.Global.Update("connection", id, state.Connected, target)
// ... on failure ...
state.Global.Update("connection", id, state.Retrying, err.Error())
// ... on cleanup — always pair Delete with DeleteMetrics ...
state.Global.Delete("forward", compID)
state.Global.DeleteMetrics("forward", compID)

// Graceful shutdown
ctx, cancel := context.WithCancel(context.Background())
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
defer signal.Stop(sigCh) // always pair Notify with Stop
go func() { <-sigCh; cancel() }()
// ... start components with ctx ...
<-ctx.Done()
wg.Wait()

// Safe close from multiple goroutines
var closeOnce sync.Once
closeCh := func() { closeOnce.Do(func() { ch.Close() }) }
defer closeCh()

// Listener close with context cancel (prevents double-close on Windows)
var lnOnce sync.Once
closeLn := func() { lnOnce.Do(func() { _ = ln.Close() }) }
defer closeLn()
stop := context.AfterFunc(ctx, closeLn)
defer stop()

// Retry with proper timer cleanup
t := time.NewTimer(retryDelay)
select {
case <-ctx.Done():
    t.Stop()
    return
case <-t.C:
}
```

## Security

- **Cap unbounded collections** from untrusted input with constants (`maxPeers`, `maxRequestBodySize`, `maxSyncFileSize`, `maxClipboardPayload`).
- **Path traversal protection** — `filepath.Base()` on user-supplied file names before any file operations.
- **No secrets in config** — use `password_command` to fetch from external tools.
- **Permission checks** — warn on world-readable config files (`warnInsecurePermissions`).
- **SSH host key verification** — `known_hosts` required; explicit opt-out via `StrictHostKeyChecking: no`.
- **Crypto** — SHA-256 only, never MD5.
- **Timeouts everywhere** — handshake, request, keepalive. No unbounded waits.

## Build

```bash
task build              # → build/mesh (version from git)
task test               # go test -count=1 ./...
task bench              # go test -bench=. -benchmem ./...
task dist               # cross-compile darwin/linux/windows
task check              # full gate: vet, staticcheck, fmt, tidy, race tests, build, e2e
FAST=1 task check       # same gate, minus the e2e lane (inner-loop iteration)
task e2e                # scenario suite against real containers (requires docker)
task e2e:churn          # heavy filesync stress suite (nightly-grade, e2e_churn tag)
task e2e:full           # e2e + e2e:churn
task e2e:compose:up     # bring up the full-topology manual playground
task e2e:compose:down   # tear it down
```

Version injected via `-ldflags -X main.version=...` from git tags. `CGO_ENABLED=0` for Linux and Windows static binaries. Darwin omits `CGO_ENABLED` to auto-detect (1 when native toolchain is available).

## End-to-End Test Harness

`task check` runs the unit suite and the e2e scenario suite by default. Release cannot ship without both gates green. `FAST=1 task check` is the inner-loop escape hatch — it still runs vet, staticcheck, fmt, tidy, the unit lane, and build, but skips the e2e lane. Docker is a hard prerequisite when the e2e lane runs.

**Layout** (see [Project Structure](#project-structure)): harness primitives in `e2e/harness/`, four scenarios in `e2e/scenarios/` (S1 SSH bastion, S2 filesync two-peer, S3 clipsync discovery, S4 gateway with stub upstream), heavy stress in `e2e/churn/`, a canned-response upstream in `e2e/stub/`, a full-topology compose playground in `e2e/compose/`.

**Build tags.** Harness files carry `//go:build e2e || e2e_churn` so both scenario tags can import them. Scenario files are `//go:build e2e`. Churn files are `//go:build e2e_churn`. The stub is `//go:build e2e`. `go build ./...` and the release binary never link any of them; testcontainers-go appears in `go.mod` but only because `go mod tidy` considers tagged files.

**Image.** `task build:e2e-image` produces `mesh-e2e:local` from `e2e/Dockerfile.e2e`: Alpine base + baked mesh binary + baked stub-llm + a fake `xclip` shim that reads and writes `/tmp/mesh-clip/<target>`. Image rebuild is content-hash-gated so repeat invocations are no-ops when nothing changed. The fake xclip lets scenarios drive clipsync through `docker exec` without any production code change.

**Networking.** Scenarios create a per-test testcontainers bridge network. Admin APIs are only bound to `127.0.0.1` inside each container (per mesh's config validator) so the harness reaches them via `docker exec curl`, not a host port mapping. Filesync's peer validator is IP-based with no DNS resolution — automated scenarios work around this with an sh wrapper that polls `getent hosts` at container start and rewrites a placeholder in the YAML before exec'ing mesh. The compose playground sidesteps it by assigning static IPs.

**Artifacts.** On test failure, `harness.DumpOnFailure` writes the container's docker log, mesh log, `/api/state`, and `/api/metrics` into `e2e/build/artifacts/<test>/<timestamp>/`. On success the test leaves nothing behind.

## What NOT to Do

- Don't add `os.Exit()` outside `cmd/mesh/main.go`
- Don't use `log.Fatal` — return errors
- Don't use MD5 — use SHA-256
- Don't store secrets in config — use `password_command`
- Don't use `time.Sleep` in tests — use channels or atomic
- Don't clear screen with `\033[2J` — use alternate screen buffer + in-place overwrite
- Don't call `Close()` from multiple goroutines without `sync.Once` protection
- Don't use `fmt.Sprintf` in hot loops — pre-compute or use `strings.Builder`
- Don't use `time.After` in select with `ctx.Done()` — use `time.NewTimer` with explicit `Stop()`
- Don't call `signal.Notify` without a matching `defer signal.Stop` — leaks signal handlers
- Don't add dependencies for things stdlib handles — exhaust stdlib first
- Don't write `\n` in raw terminal mode — use `\r\n` (kernel `NL→CRNL` translation is disabled)
- Don't call blocking functions (like `sftp.Serve()`) inline in channel request loops — use a goroutine
- Don't assume compile-time constants are correct when the value they derive from becomes configurable — recompute at startup
- Don't change a function signature without checking every call site AND every test helper that constructs the struct

## Process Rules

**Regression tests are mandatory.** Every bug fix must include a test that reproduces the exact failure scenario. Every new feature must include tests that pin its behavior. Tests are the only reliable way to prevent regressions when working on unrelated changes.

**Post-batch review is mandatory.** After completing a batch of changes (features, refactors, or fixes), review all modified code for collateral damage before considering the work done. Focused work on one area routinely breaks adjacent code — this has happened repeatedly in this project. Use `feature-dev:code-reviewer` agents for independent review.

**Commit after each logical change.** Don't accumulate changes across multiple features into one commit. Each feature, each bug fix, each test addition is its own commit. This makes bisecting and reverting possible.
