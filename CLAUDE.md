# mesh — Architecture & Conventions

> For AI assistants and new contributors. Read this before making changes.

## What is mesh?

A single-binary, cross-platform networking tool that replaces `ssh`, `sshd`, `autossh`, `socat`, and SOCKS/HTTP proxy servers. One config file defines listeners, outbound connections, port forwards, and clipboard sync.

## Project Structure

```
cmd/mesh/           CLI entry point, dashboard, status rendering
internal/
  config/           YAML config loading, validation, structs
  tunnel/           SSH client + server, port forwarding, keepalives
  proxy/            SOCKS5 + HTTP CONNECT proxy servers
  netutil/          TCP helpers (BiCopy, keepalive, reusable listeners)
  clipsync/         Clipboard sync (UDP discovery, HTTP push/pull, OS clipboard I/O)
  state/            Thread-safe component state (Global singleton with Snapshot())
configs/            Example YAML + JSON schema
```

## Key Design Decisions

**Single `state.Global`** — All components (listeners, connections, forwards, clipsync) update a shared `state.State` via `Update/Delete`. The dashboard and status commands read via `Snapshot()` which returns a copy. Mutex-protected.

**ForwardSet = independent SSH connection** — Each `ForwardSet` within a `Connection` gets its own physical SSH connection for throughput isolation. This is intentional, not a bug.

**Multiplex mode** — `mode: multiplex` on a Connection connects to ALL targets simultaneously (one SSH connection per target). Default `failover` mode tries targets in order.

**Auth method order** — `buildAuthMethods()` tries: SSH agent → private key → password_command. Multiple methods can be configured; they're all offered to the server.

**Clipsync protocol** — Push-based via HTTP POST. The sender embeds file data directly in the payload for one-way connectivity (receiver can't pull back). Pull-back via `/files/` endpoint is a fallback for when the receiver CAN reach the sender. UDP discovery uses broadcast beacons with unicast reply for asymmetric networks.

**Dashboard** — Uses terminal alternate screen buffer (`\033[?1049h`). Overwrites in-place line by line (`\033[K` per line, `\033[J` to clear remainder). No scrollback pollution, no flicker.

**Config precedence** — Hardcoded defaults → config file (YAML) → environment variables (`os.ExpandEnv` in config loading) → CLI flags. Validation at load time with actionable errors.

## CLI

```
mesh [-f config.yaml] <node> <command> [arguments]
```

| Command | Purpose |
|---|---|
| `up` | Start node with live dashboard (alternate screen when TTY) |
| `down` | Stop running node (SIGTERM + wait) |
| `status [-w]` | Show node status; `-w` for watch mode |
| `config` | Show parsed config without starting |
| `completion` | Generate shell completions (bash, zsh, fish) |
| `help` | Detailed help with SSH options |

Shell completions dynamically resolve node names from the config file.

## Conventions

- **ANSI colors** — Package-level vars (`cReset`, `cBold`, etc.), disabled when `NO_COLOR` is set.
- **Address sorting** — `compareAddr()` with `addrKey` pre-parsed into `uint64` pairs for zero-allocation IPv4 comparison.
- **Log handler chain** — `humanLogHandler` wraps tint. In dashboard mode: `humanLogHandler → multiHandler → {tint(file), tint(ring)}`.
- **Platform code** — Build-tagged files (`_unix.go`, `_windows.go`), never `runtime.GOOS` in core logic.
- **Config validation** — `validate()` checks at load time. Auth requires at least one method. `known_hosts` or `StrictHostKeyChecking: no` required for SSH.
- **Tests** — Table-driven. Real TCP/HTTP for integration tests (`httptest.NewServer`). No `time.Sleep` — use channels/atomic. Race detector always on.
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

// Graceful shutdown
ctx, cancel := context.WithCancel(context.Background())
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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

- **Cap unbounded collections** from untrusted input with constants (`maxPeers`, `maxRequestBodySize`, `maxSyncFileSize`).
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
- Don't add dependencies for things stdlib handles — exhaust stdlib first
