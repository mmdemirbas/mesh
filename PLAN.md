# PLAN.md

Roadmap for mesh. Last verified on 2026-04-07.
Items ordered by priority within each tier.

---

## Tier 1 — Bugs & Resource Leaks

| ID  | Component | Item                                                  | Status | Notes                                                                                                                                                                           |
|-----|-----------|-------------------------------------------------------|--------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| B4  | clipsync  | `pullHTTP` HTTP body not closed on non-200            | FIXED  | Moved `defer resp.Body.Close()` before status check.                                                                                                                            |
| B5  | clipsync  | `downloadFile` HTTP body not closed on non-200        | FIXED  | Same fix as B4. Also fixed nil error return on non-200.                                                                                                                          |
| B6  | tunnel    | SSH agent socket FD never closed                      | FIXED  | `buildAuthMethods` now returns a cleanup function. Callers defer it.                                                                                                             |
| B7  | filesync  | HTTP requests not context-aware; 10-min shutdown hang | FIXED  | All outgoing HTTP calls now use `http.NewRequestWithContext`. Context threaded through `sendIndex`, `postIndex`, `downloadFile`, `downloadFileDelta`.                             |
| B8  | filesync  | `.mesh-delta-tmp` orphan files never cleaned          | FIXED  | `cleanTempFiles` now matches both `.mesh-tmp-` prefix and `.mesh-delta-tmp` suffix.                                                                                              |
| B9  | proxy     | Relay accept loop missing backoff on EMFILE           | FIXED  | Added 50ms context-aware backoff, matching `ServeSocks` and `ServeHTTPProxy`.                                                                                                    |
| B10 | tunnel    | `authFailuresByIP` grows without bound                | FIXED  | Changed value type to track `lastSeen` timestamp. `evictOldAuthFailures` runs alongside limiter eviction.                                                                        |
| B11 | tunnel    | `limiter.Wait(context.Background())` holds goroutine  | FIXED  | Now uses server `ctx` so goroutines unblock on shutdown.                                                                                                                         |
| B12 | tunnel    | Race: cancel-tcpip-forward vs listener registration   | FIXED  | Mutex now held across `net.Listen` + map insertion.                                                                                                                              |
| B13 | tunnel    | Cleanup goroutines outlive listener; stale state      | FIXED  | `Delete`/`DeleteMetrics` moved to defer, runs on any exit path. `context.AfterFunc` replaces manual goroutine.                                                                   |
| B14 | filesync  | Pending exchange `\|resp` cache never evicted         | FIXED  | Added periodic `evictStalePending` goroutine (1-minute interval) that cleans both upload and response caches older than `pendingTTL`.                                             |
| B15 | filesync  | `http.Transport` not closed on shutdown               | FIXED  | `CloseIdleConnections()` called after `wg.Wait()`.                                                                                                                               |
| B16 | state     | Components/metrics maps grow without bound            | OPEN   | Needs design decision: TTL, cap, or enforced pairing convention.                                                                                                                 |
| B17 | filesync  | `out` file closed by defer, not before rename         | FIXED  | File explicitly closed before return so caller can rename on Windows.                                                                                                            |
| B18 | shell     | Windows: SSH channel not closed on `cmd.Start` fail   | FIXED  | Added `closeCh()` call on Start failure, matching Unix behavior.                                                                                                                 |
| FS6 | tunnel    | `CountedBiCopy` tx/rx labels swapped                  | FIXED  | Swapped counter assignment so tx counts a→b and rx counts b→a as documented.                                                                                                     |
| B3  | clipsync  | Clipboard overwritten without user intent             | OPEN   | Needs design. No inbound write gate — any peer can push at any time.                                                                                                             |

---

## Tier 2 — Security

| ID  | Component | Item                                     | Notes                                                                                                                                                                                                                 |
|-----|-----------|------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| S2  | filesync  | Unbounded `totalPages` from remote peers | FIXED. Capped at `maxTotalPages` (200). Requests exceeding the cap are rejected with 400.                                                                                                                             |
| S1  | clipsync  | No TLS for clipsync HTTP                 | Needs design. Options: mTLS, pre-shared key over TLS, or opportunistic TLS with self-signed certs. Config schema needs `tls_cert`/`tls_key` fields. Must remain backward-compatible for localhost-only setups.        |
| FS4 | filesync  | No TLS / auth for filesync HTTP          | Same design as S1 — share the solution. Peer validation is IP-only for non-loopback; loopback is now trusted (tunnel provides auth). Any machine with the right IP gets full read/write access on direct connections. |

---

## Tier 3 — Testing

| ID | Component | Item                                   | Notes                                                                                                                                         |
|----|-----------|----------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| T2 | tunnel    | Tunnel package coverage gaps           | Remaining gaps: `runLocalForward`, `runRemoteForward`, `buildAuthMethods`, full SSH client lifecycle, multiplex mode, `ExitOnForwardFailure`. |
| T3 | all       | Integration tests: real SSH + clipsync | Full client-server SSH roundtrip. Clipsync push/pull between two in-process nodes.                                                            |
| T4 | proxy     | Non-CONNECT HTTP forward path untested | `http.go:90-125`. Exercises its own `dialer` call, `bufio.Reader` wrapping, and `BiCopy`. No test coverage.                                   |

---

## Tier 4 — Features

Ordered by estimated value and complexity. Each needs design before implementation.

| ID  | Component | Item                    | Complexity  | Notes                                                                                                                                                                         |
|-----|-----------|-------------------------|-------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| F15 | core      | Self-monitoring metrics | Low         | Expose open FD count, `runtime.NumGoroutine()`, state map sizes on `/api/metrics`. Log warning when thresholds exceeded. Lightweight — no new dependencies.                   |
| F13 | clipsync  | Payload size limit      | Low         | Network side partially capped (`maxRequestBodySize`). Gap: local clipboard read has no cap. A large image can OOM the sender. Add a per-format size check in `pollClipboard`. |
| FS5 | filesync  | Outgoing delta index    | Medium      | `buildIndexExchange(folderID, 0)` always sends full index. Subsequent syncs could send only entries newer than `LastSentSequence`. Requires per-peer sent state tracking.     |
| F7  | sshd      | Env var forwarding      | Low         | Handle `"env"` request type. Collect before `"shell"/"exec"`, apply configurable allowlist (`AcceptEnv`), append to `cmd.Env`.                                                |
| F9  | sshd      | Exit-signal reporting   | Low         | Check `WaitStatus.Signaled()`, map to SSH signal name, send `exit-signal` instead of `exit-status`.                                                                           |
| F10 | sshd      | Banner and MOTD         | Low         | `ssh.ServerConfig.BannerCallback` for pre-auth. Channel data write before shell for post-auth MOTD. Config: `banner` and `motd` fields.                                       |
| F8  | sshd      | Signal forwarding       | Medium      | Handle `"signal"` request type for non-PTY sessions. Map SSH signal names to `syscall.Signal`. Send to process group via `syscall.Kill(-pgid, sig)`.                          |
| F12 | sshd      | Windows shell default   | Decision    | Current: `cmd.exe` via `COMSPEC`. PowerShell (`pwsh.exe`) is modern but not universally available. Decide and document. No ConPTY support yet.                                |
| F2  | cli       | `mesh init` command     | Medium      | Interactive config generator. Scaffolds a starter YAML with common patterns.                                                                                                  |
| F5  | sshd      | SFTP subsystem          | Medium      | Handle `"subsystem"` request with `sftp` name. Requires `github.com/pkg/sftp` dependency (new). Enables `scp`, `sftp`, `rsync`.                                               |
| F6  | sshd      | SSH agent forwarding    | Medium      | Handle `auth-agent-req@openssh.com`. Create per-session Unix socket, set `SSH_AUTH_SOCK`. Unix-only.                                                                          |
| F3  | cli       | SSH client subcommands  | Medium-High | Emulate `ssh` CLI for one-off connections without YAML. Needs argument parsing, ephemeral config construction.                                                                |
| F4  | sshd      | User switching          | High        | `setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows. Requires root/capabilities. Security-critical.                                                                   |
| F1  | core      | Config hot-reload       | High        | File watcher, config diff, per-component context tree with independent cancellation. Currently all components share one root context with no restart capability.              |
| F11 | sshd      | X11 forwarding          | High        | Xauth cookie handling, Unix socket, channel multiplexing. Low demand.                                                                                                         |
| F14 | gateway   | LLM API gateway         | High        | Bidirectional translation between Anthropic and OpenAI API formats. Detailed plan in [GATEWAY_PLAN.md](GATEWAY_PLAN.md).                                                      |

---

## Tier 5 — Release / packaging

| ID | Component | Item                         | Notes                                                                     |
|----|-----------|------------------------------|---------------------------------------------------------------------------|
| R1 | release   | Semantic versioning          | Tag `v0.1.0` or `v1.0.0`. Define stability commitment.                    |
| R2 | release   | CHANGELOG.md                 | Start from current state.                                                 |
| R3 | release   | Verify `go install` path     | End-to-end test: `go install github.com/mmdemirbas/mesh/cmd/mesh@latest`. |
| R4 | docs      | README: admin server docs    | Port file location, API endpoints, one curl example.                      |
| R5 | docs      | README: demo GIF             | Capture live dashboard in action.                                         |
| R6 | release   | Homebrew formula             |                                                                           |
| R7 | release   | Dockerfile                   |                                                                           |
| R8 | release   | systemd unit + launchd plist |                                                                           |

---

## Code quality (from resource audit 2026-04-07)

Low-severity items. Not bugs, but worth cleaning up. Grouped by area.

**tunnel:**

- Pre-compute `ak.Marshal()` at load time instead of per-auth-attempt (`tunnel.go:83`)

**filesync:**

- Stop debounce timer on context cancel (`watcher.go:96-103`)
- Remove unused `fw.mu` field (`watcher.go:27`) — dead code
- Log inotify watch failures at Warn, not Debug (`watcher.go:70`) — silent coverage loss

**proxy:**

- Use `time.NewTimer` + `Stop()` instead of `time.After` in accept-error paths (`socks5.go:25`,
  `http.go:57`)
- Eliminate double-close patterns in HTTP and SOCKS5 handlers (`http.go:139`, `socks5.go:76`)

**clipsync:**

- Remove duplicate `runtime.GOOS == "windows"` check in `runUDPBeacon` (`clipsync.go:1434`)
- Use node context for clipboard subprocess timeouts (`clipsync.go:842`)

**cmd:**

- Call `signal.Stop` for signal/SIGWINCH handlers (`main.go:348`, `sigwinch_unix.go:12`)
- Remove unnecessary `time.Sleep(50ms)` in shutdown path (`main.go:476`)
- Reduce allocations in `humanLogHandler.Handle` — 2 map allocs per log record (`main.go:496`)

---

## Pre-existing flaky test

`TestAcceptAndForward_DialerErrorDropsConnection` occasionally fails with "connection reset by peer"
on `net.Dial` due to `SetLinger(0)` on accepted connections.

## Recent findings (2026-04-07)

### Filesync over SSH tunnels broken — FIXED

Root cause: `handleIndex`, `handleFile`, `handleDelta` validated peers by matching the HTTP
request's source IP against configured peer addresses. SSH-tunneled connections always arrive
from `127.0.0.1`, which doesn't match the remote peer's real IP on the receiving end.
Result: 403 rejection, but the HTTP response arrives while the client is still streaming
the POST body, so the client sees `write tcp: broken pipe` instead of the actual 403.

Fix: Loopback connections (127.0.0.1, ::1) are now trusted unconditionally — the SSH tunnel
is the authentication boundary. Non-loopback connections still require IP match.
Peer rejections now log at Warn level so they appear in the dashboard log ring.

### Large index exchange broken — FIXED

Root cause: Folders with 110K+ files produce a protobuf index >10 MB, exceeding
`maxIndexPayload`. The receiver reads up to 10 MB via `LimitReader`, the truncated
protobuf fails to unmarshal, and the server closes the connection mid-transfer.

Fix: Paginated index exchange. Indices with >10K files are split into pages (~1.3 MB each),
sent as sequential HTTP requests. Server accumulates pages, processes the full index after
the final page, and returns its own response (also paginated if needed). Single-page
exchange is unchanged (backward compatible).

## My Notes

- filesync - copy metadata from all 3 computers and generate filesync equivalents. Resolve existing
  conflicts. Sync ignores. Test end to end.
- gitignore vendor dir
- show last clipboard activity (direction, size, mime, time)
- clipsync - file/image copy support