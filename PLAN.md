# PLAN.md

Roadmap for mesh. Last verified on 2026-04-06.
Items ordered by priority within each tier.

---

## Tier 1 — Bugs

| ID | Component | Item                                      | Notes                                                                                                                                                                                                   |
|----|-----------|-------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| B3 | clipsync  | Clipboard overwritten without user intent | Needs design. No inbound write gate — any allowed peer can push at any time. Possible mitigations: rate-limit inbound writes, receive window, or configurable sync direction. Needs reproduction first. |

---

## Tier 2 — Security

| ID  | Component | Item                            | Notes                                                                                                                                                                                                          |
|-----|-----------|---------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| S1  | clipsync  | No TLS for clipsync HTTP        | Needs design. Options: mTLS, pre-shared key over TLS, or opportunistic TLS with self-signed certs. Config schema needs `tls_cert`/`tls_key` fields. Must remain backward-compatible for localhost-only setups. |
| FS4 | filesync  | No TLS / auth for filesync HTTP | Same design as S1 — share the solution. Peer validation is IP-only for non-loopback; loopback is now trusted (tunnel provides auth). Any machine with the right IP gets full read/write access on direct connections. |

---

## Tier 3 — Testing

| ID | Component | Item                                    | Notes                                                                                                                                         |
|----|-----------|-----------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| T2 | tunnel    | Tunnel package coverage gaps            | Remaining gaps: `runLocalForward`, `runRemoteForward`, `buildAuthMethods`, full SSH client lifecycle, multiplex mode, `ExitOnForwardFailure`. |
| T3 | all       | Integration tests: real SSH + clipsync  | Full client-server SSH roundtrip. Clipsync push/pull between two in-process nodes.                                                            |

---

## Tier 4 — Features

Ordered by estimated value and complexity. Each needs design before implementation.

| ID  | Component | Item                      | Complexity  | Notes                                                                                                                                                                         |
|-----|-----------|---------------------------|-------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| F13 | clipsync  | Payload size limit        | Low         | Network side partially capped (`maxRequestBodySize`). Gap: local clipboard read has no cap. A large image can OOM the sender. Add a per-format size check in `pollClipboard`. |
| F7  | sshd      | Env var forwarding        | Low         | Handle `"env"` request type. Collect before `"shell"/"exec"`, apply configurable allowlist (`AcceptEnv`), append to `cmd.Env`.                                                |
| F9  | sshd      | Exit-signal reporting     | Low         | Check `WaitStatus.Signaled()`, map to SSH signal name, send `exit-signal` instead of `exit-status`.                                                                           |
| F10 | sshd      | Banner and MOTD           | Low         | `ssh.ServerConfig.BannerCallback` for pre-auth. Channel data write before shell for post-auth MOTD. Config: `banner` and `motd` fields.                                       |
| F8  | sshd      | Signal forwarding         | Medium      | Handle `"signal"` request type for non-PTY sessions. Map SSH signal names to `syscall.Signal`. Send to process group via `syscall.Kill(-pgid, sig)`.                          |
| F12 | sshd      | Windows shell default     | Decision    | Current: `cmd.exe` via `COMSPEC`. PowerShell (`pwsh.exe`) is modern but not universally available. Decide and document. No ConPTY support yet.                                |
| F2  | cli       | `mesh init` command       | Medium      | Interactive config generator. Scaffolds a starter YAML with common patterns.                                                                                                  |
| F5  | sshd      | SFTP subsystem            | Medium      | Handle `"subsystem"` request with `sftp` name. Requires `github.com/pkg/sftp` dependency (new). Enables `scp`, `sftp`, `rsync`.                                               |
| F6  | sshd      | SSH agent forwarding      | Medium      | Handle `auth-agent-req@openssh.com`. Create per-session Unix socket, set `SSH_AUTH_SOCK`. Unix-only.                                                                          |
| F3  | cli       | SSH client subcommands    | Medium-High | Emulate `ssh` CLI for one-off connections without YAML. Needs argument parsing, ephemeral config construction.                                                                |
| F4  | sshd      | User switching            | High        | `setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows. Requires root/capabilities. Security-critical.                                                                   |
| F1  | core      | Config hot-reload         | High        | File watcher, config diff, per-component context tree with independent cancellation. Currently all components share one root context with no restart capability.              |
| F11 | sshd      | X11 forwarding            | High        | Xauth cookie handling, Unix socket, channel multiplexing. Low demand.                                                                                                         |
| F14 | gateway   | LLM API gateway           | High        | Bidirectional translation between Anthropic and OpenAI API formats. Detailed plan in [GATEWAY_PLAN.md](GATEWAY_PLAN.md). |

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

### Open items — not yet addressed

| ID   | Component | Item                            | Notes |
|------|-----------|---------------------------------|-------|
| FS5  | filesync  | Outgoing delta index            | `buildIndexExchange(folderID, 0)` always sends the full index. Subsequent syncs could send only entries newer than `LastSentSequence`. Requires tracking per-peer sent state and redesigning `diff()` to handle incremental remote input. Does not help first sync. |
| FS6  | filesync  | `CountedBiCopy` tx/rx labels    | Comment says "tx counts a→b" but code counts tx=b→a. Dashboard ↑↓ arrows may be swapped for all tunnel metrics. Cosmetic but misleading for debugging. |

## My Notes

- filesync - copy metadata from all 3 computers and generate filesync equivalents. Resolve existing
  conflicts. Sync ignores. Test end to end.
