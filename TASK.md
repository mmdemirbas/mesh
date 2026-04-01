# TASK.md

Consolidated task list. Items verified against source code on 2026-04-01.
Priority order within each section: highest risk or highest value first.

---

## Bugs

| # | Item | Location | Notes |
|---|------|----------|-------|
| B1 | Dynamic metrics leak: `DeleteMetrics` never called for "dynamic" entries in production | `tunnel.go` around the `state.Global.Delete("dynamic", compID)` call; also forward cleanup paths | Add `state.Global.DeleteMetrics(...)` alongside each `state.Global.Delete(...)` call. Same pattern for "forward" orphaned entries at the 4 forward cleanup paths (bounded by config, lower priority). |
| B2 | Duplicate UDP close goroutines: 3 `conn.Close()` paths on the same UDP socket | `clipsync/clipsync.go` around lines 1303-1314 | `defer conn.Close()` + two goroutines each doing `<-ctx.Done(); conn.Close()`. Remove duplicates; keep one `sync.Once`-guarded close following project convention. |
| B3 | Clipboard content changes unexpectedly when clipsync is not actively in use | `clipsync/clipsync.go` | Symptom reported. Root cause unknown. Needs reproduction and diagnosis before fix. |

---

## Security

| # | Item | Location | Notes |
|---|------|----------|-------|
| S1 | TLS for clipsync HTTP | `clipsync/clipsync.go` — HTTP push/pull endpoints and server | Highest remaining security gap. Needs design: mTLS vs pre-shared key, cert management, config schema, backward compat for local-only setups. |
| S2 | Slowloris protection missing for `runRemoteProxy` | `proxy/http.go:70`, `proxy/socks5.go:37` | `SetDeadline` is a no-op on SSH channels. Replace with a context-based or goroutine-based timeout for the handshake phase only. Works on all connection types. |
| S3 | Admin server startup failure is silent | `cmd/mesh/main.go` around the admin server init | If `admin_addr` is invalid, server fails to start silently and port file is not written. Add a warning log. |

---

## Testing

| # | Item | Location | Notes |
|---|------|----------|-------|
| T1 | Admin server startup tests missing | `cmd/mesh/main_test.go` | Test: random port binding when `admin_addr == ""`; disabled when `"off"`; port file created and cleaned up; multiple nodes write correct port files. |
| T2 | Tunnel package coverage is 17.9% | `internal/tunnel/tunnel_test.go` | Priority test scenarios: SSH server auth (valid/invalid key, timing), keepalive, forward failure (broken target, timeout, mid-stream disconnect), rate limiter edge cases. Use real `net.Listener`. |
| T3 | Integration tests: real SSH handshake + clipsync | New test file(s) | Full client↔server SSH roundtrip. Clipsync push/pull between two in-process nodes. |
| T4 | Fuzz tests for parsers | `cmd/mesh/` | `go test -fuzz` for `parseIPv4Port`, `parseByteSize`, `parseTarget`, and config YAML loading. |

---

## Code Quality

| # | Item | Location | Notes |
|---|------|----------|-------|
| Q1 | Errcheck: unchecked `Close`/`Remove` errors in clipsync production code | `clipsync/clipsync.go` | LINT_PLAN item #23: still todo. Handle or explicitly suppress with `_ =` + comment. |
| Q2 | Errcheck: unchecked `Close` errors in test files | Various `*_test.go` | LINT_PLAN item #24: still todo. |
| Q3 | HTTP proxy parse error is silent | `proxy/http.go:73-76` | Malformed request returns silently with no log. Add a DEBUG log with the error. Helps diagnose config errors, TLS-to-plaintext mistakes, port scanning. |
| Q4 | PermitOpen separator: comma vs space undecided | `tunnel/tunnel.go` around `strings.Split(permitOpen, ",")` | OpenSSH uses spaces. Decide and document. Consider supporting both. Config docs should show examples. |
| Q5 | SetLinger(0) risk: RST on close may discard unsent data | `netutil/netutil.go:23` | Applied to all accepted TCP connections. On localhost the risk is negligible; on real networks small. Evaluate removing; verify `SO_REUSEADDR` on listeners is sufficient for port reuse. |
| Q6 | Standardize "relay" vs "forward" terminology | Config, logs, code comments | Pick one term and apply consistently. |

---

## Energy / Performance

| # | Item | Location | Notes |
|---|------|----------|-------|
| E1 | Clipboard poll interval: make configurable, raise default | `clipsync/clipsync.go:32` (`PollInterval = 2s`) | ~1800 subprocess forks/hour idle on macOS/Linux. Add `poll_interval` to clipsync config; raise default to 5s. Event-driven clipboard (no subprocess) would need cgo or external runtime libs — not acceptable given single-binary constraint. |
| E2 | Dashboard: skip render when state unchanged | `cmd/mesh/dashboard.go` | 60 wakes/min even when nothing changes. Compare snapshot to previous frame before writing to terminal. |
| E3 | UDP beacon: increase periodic interval to 10s | `clipsync/clipsync.go:1348` | Current 3s; peer TTL is 15s so 10s is safe. Event-driven path via `notifyCh` already handles clipboard changes immediately. |

---

## Features

| # | Item | Notes |
|---|------|-------|
| F1 | Config hot-reload | Watch config file, diff changes, restart affected components. Needs lifecycle management. |
| F2 | `mesh init` command | Generate a starter config interactively. |
| F3 | SSH client subcommands | Emulate `ssh` CLI to run one-off connections without YAML changes. |
| F4 | sshd: user switching | Run shell as authenticated user (`setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows). Requires root or `CAP_SETUID`/`CAP_SETGID`. |
| F5 | sshd: SFTP subsystem | Handle `subsystem sftp` channel requests. Enables `scp`, `sftp`, `rsync`. |
| F6 | sshd: SSH agent forwarding | Handle `auth-agent-req@openssh.com` and `auth-agent@openssh.com`. Creates per-session Unix socket, sets `SSH_AUTH_SOCK`. |
| F7 | sshd: environment variables from client | Handle `env` session requests. Merge `LANG`, `LC_*` etc. with configurable allowlist (like `AcceptEnv`). |
| F8 | sshd: signal forwarding | Handle `signal` session requests for non-PTY sessions (e.g., `ssh host 'long-command'` then Ctrl+C). |
| F9 | sshd: exit-signal reporting | Send `exit-signal` when shell killed by signal, not just `exit-status`. |
| F10 | sshd: banner and MOTD | Support `Banner` (pre-auth) and MOTD (post-auth) config options. |
| F11 | sshd: X11 forwarding | Handle `x11-req` and `x11` channel type. Requires Xauth cookie handling. Low priority. |
| F12 | Windows shell default | Decide: PowerShell (modern, becoming standard) vs cmd (historical sshd default). |
| F13 | Clipsync size limit | Cap max payload size to prevent OOM on large clipboard content. |

---

## Release / Packaging

| # | Item | Notes |
|---|------|-------|
| R1 | Semantic versioning | Establish `v1.0.0` baseline tag. Decide what "stable" means for this project. |
| R2 | CHANGELOG.md | Start changelog from current state. |
| R3 | Verify `go install github.com/mmdemirbas/mesh/cmd/mesh@latest` | End-to-end install test. |
| R4 | README: admin server documentation | Show how to find the port file and call the API. One example is enough. |
| R5 | README: demo GIF | Live dashboard in action. |
| R6 | Homebrew formula | |
| R7 | Dockerfile | |
| R8 | systemd unit file + launchd plist | |
