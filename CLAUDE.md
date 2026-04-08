# mesh — Architecture & Conventions

> For AI assistants and new contributors. Read this before making changes.

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
  state/            Thread-safe component state (Global singleton with Snapshot())
configs/            Example YAML, production config (mesh.yaml), JSON schema
```

## Key Design Decisions

**Single `state.Global`** — All components (listeners, connections, forwards, clipsync) update a shared `state.State` via `Update/Delete`. The dashboard and status commands read via `Snapshot()` which returns a copy. Mutex-protected. Each component tracks `LastUpdated`; a background goroutine (`StartEviction`, 5-min interval) removes entries not updated within 1 hour and their orphaned metrics.

**ForwardSet = independent SSH connection** — Each `ForwardSet` within a `Connection` gets its own physical SSH connection for throughput isolation. This is intentional, not a bug.

**Multiplex mode** — `mode: multiplex` on a Connection connects to ALL targets simultaneously (one SSH connection per target). Default `failover` mode tries targets in order.

**Auth method order** — `buildAuthMethods()` tries: SSH agent → private key → password_command. Multiple methods can be configured; they're all offered to the server. Authorized keys are pre-marshaled at load time to avoid repeated marshaling on every auth attempt.

**SSH server features** — `accept_env` config field controls which client environment variables are accepted (supports trailing wildcards like `LC_*`). `banner` and `motd` config fields point to text files read at startup: banner is shown pre-auth via `ssh.ServerConfig.BannerCallback`, MOTD is written to the session channel post-auth before shell I/O starts. Exit-signal reporting per RFC 4254 section 6.10 — processes killed by a signal send `exit-signal` with the mapped signal name instead of `exit-status` with code 0. Signal-to-name mapping in `env.go`. Windows default shell prefers `pwsh.exe` (modern PowerShell) if installed, falls back to `COMSPEC`/`cmd.exe`. Exec commands use `-Command` for PowerShell, `/C` for cmd.exe.

**Clipsync protocol** — Push-based via HTTP POST with gzip-compressed protobuf serialization (`SyncPayload`, `Beacon`, `DiscoverRequest` in `clipsync/proto/`). `Content-Encoding: gzip` header signals compression on both requests and responses. The sender embeds file data directly in the payload for one-way connectivity (receiver can't pull back). Pull-back via `/files/` endpoint is a fallback for when the receiver CAN reach the sender. UDP discovery uses broadcast beacons (10s interval) with unicast reply for asymmetric networks. Group isolation via `lan_discovery_group` config field (list of strings) — peers with no overlapping group ignore each other; empty list disables dynamic discovery entirely. Clipboard polling interval is configurable via `poll_interval` (default 3s). Local clipboard reads are capped at `maxClipboardPayload` (100 MB total across all formats) to prevent OOM.

**Dashboard** — Uses terminal alternate screen buffer (`\033[?1049h`). Overwrites in-place line by line (`\033[K` per line, `\033[J` to clear remainder). Header (uptime/clock) always written; body (status + logs) skipped when unchanged. No scrollback pollution, no flicker.

**Config precedence** — Hardcoded defaults → config file (YAML) → environment variables (`os.ExpandEnv` in config loading) → CLI flags. Validation at load time with actionable errors.

**Filesync** — Folder sync with named peer definitions and config-level defaults. Peers are declared as a `peers:` map (name → addresses) at the filesync level; folders reference peer names. A `defaults:` section provides fallback values (peers, direction, ignore patterns) that individual folders can override (peers, direction) or extend (ignore patterns are appended). Config is resolved at load time: `FilesyncCfg.Resolve()` merges defaults, resolves peer names to addresses, and populates `ResolvedFolders []FolderCfg`. Runtime code reads only `ResolvedFolders`. Gzip-compressed protobuf index exchange with delta mode (`since` field skips unchanged entries); `Content-Encoding: gzip` header on both requests and responses. Outgoing index is also delta: `PeerState.LastSentSequence` tracks what was already sent to each peer, so subsequent syncs transmit only new entries. Block-level delta transfer via `POST /delta` — receiver sends SHA-256 block signatures (128 KB blocks), sender returns only changed blocks. Bandwidth throttling via `max_bandwidth` config (token-bucket rate limiter). Dual change detection: fsnotify for real-time + periodic scan as safety net. Direction modes: `send-receive` (default), `send-only`, `receive-only`, `dry-run` (scan + compare without file changes), `disabled` (no activity, visible in dashboard). Conflict resolution uses `.sync-conflict-*` naming. Transfer resume via `.mesh-tmp-*` temp files with offset-based HTTP GET. Index persisted as YAML in `~/.mesh/filesync/<folder-id>/`. Ignore patterns are gitignore-style, configured in YAML only (no `.stignore` files). Web UI at `/ui/filesync` on the admin port.

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
- **Platform code** — Build-tagged files (`_unix.go`, `_windows.go`), never `runtime.GOOS` in core logic.
- **Config validation** — `validate()` checks at load time. Auth requires at least one method. `known_hosts` or `StrictHostKeyChecking: no` required for SSH.
- **Tests** — Table-driven. Real TCP/HTTP for integration tests (`httptest.NewServer`). Fuzz tests for parsers (`go test -fuzz`). No `time.Sleep` — use channels/atomic. Race detector always on.
- **Error handling** — Wrap errors with context: `fmt.Errorf("connections[%d] %q: %w", i, name, err)`. Return errors from libraries; never `log.Fatal` outside `main()`.
- **Resource cleanup** — `defer` for connections, file handles, goroutines. Every goroutine has a clear exit path tied to context cancellation or channel close.
- **Channel close safety** — Use `sync.Once`-guarded close functions when multiple goroutines may close the same channel/connection (see `shell_windows.go`, `shell_unix.go`).

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
```

Version injected via `-ldflags -X main.version=...` from git tags. `CGO_ENABLED=0` for static binaries.

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
