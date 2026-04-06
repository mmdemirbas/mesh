# PLAN.md

Roadmap for mesh. Last verified on 2026-04-06.
Items ordered by priority within each tier.

---

## Tier 1 — Bugs

| ID | Item                                      | Location                       | Notes                                                                                                                                                                                                   |
|----|-------------------------------------------|--------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| B3 | Clipboard overwritten without user intent | `clipsync.go` `processPayload` | Needs design. No inbound write gate — any allowed peer can push at any time. Possible mitigations: rate-limit inbound writes, receive window, or configurable sync direction. Needs reproduction first. |

---

## Tier 2 — Security

| ID | Item                     | Location                      | Notes                                                                                                                                                                                                          |
|----|--------------------------|-------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| S1 | No TLS for clipsync HTTP | `clipsync.go` `runHTTPServer` | Needs design. Options: mTLS, pre-shared key over TLS, or opportunistic TLS with self-signed certs. Config schema needs `tls_cert`/`tls_key` fields. Must remain backward-compatible for localhost-only setups. |

---

## Tier 3 — Testing

| ID | Item                                             | Location                         | Notes                                                                                                                                         |
|----|--------------------------------------------------|----------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| T2 | Tunnel package coverage gaps                     | `internal/tunnel/tunnel_test.go` | Remaining gaps: `runLocalForward`, `runRemoteForward`, `buildAuthMethods`, full SSH client lifecycle, multiplex mode, `ExitOnForwardFailure`. |
| T3 | Integration tests: real SSH handshake + clipsync | New test file(s)                 | Full client-server SSH roundtrip. Clipsync push/pull between two in-process nodes.                                                            |

---

## Tier 4 — Features

Ordered by estimated value and complexity. Each needs design before implementation.

| ID  | Item                        | Complexity  | Notes                                                                                                                                                                         |
|-----|-----------------------------|-------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| F13 | Clipsync payload size limit | Low         | Network side partially capped (`maxRequestBodySize`). Gap: local clipboard read has no cap. A large image can OOM the sender. Add a per-format size check in `pollClipboard`. |
| F7  | sshd: env var forwarding    | Low         | Handle `"env"` request type. Collect before `"shell"/"exec"`, apply configurable allowlist (`AcceptEnv`), append to `cmd.Env`.                                                |
| F9  | sshd: exit-signal reporting | Low         | Check `WaitStatus.Signaled()`, map to SSH signal name, send `exit-signal` instead of `exit-status`.                                                                           |
| F10 | sshd: banner and MOTD       | Low         | `ssh.ServerConfig.BannerCallback` for pre-auth. Channel data write before shell for post-auth MOTD. Config: `banner` and `motd` fields.                                       |
| F8  | sshd: signal forwarding     | Medium      | Handle `"signal"` request type for non-PTY sessions. Map SSH signal names to `syscall.Signal`. Send to process group via `syscall.Kill(-pgid, sig)`.                          |
| F12 | Windows shell default       | Decision    | Current: `cmd.exe` via `COMSPEC`. PowerShell (`pwsh.exe`) is modern but not universally available. Decide and document. No ConPTY support yet.                                |
| F2  | `mesh init` command         | Medium      | Interactive config generator. Scaffolds a starter YAML with common patterns.                                                                                                  |
| F5  | sshd: SFTP subsystem        | Medium      | Handle `"subsystem"` request with `sftp` name. Requires `github.com/pkg/sftp` dependency (new). Enables `scp`, `sftp`, `rsync`.                                               |
| F6  | sshd: SSH agent forwarding  | Medium      | Handle `auth-agent-req@openssh.com`. Create per-session Unix socket, set `SSH_AUTH_SOCK`. Unix-only.                                                                          |
| F3  | SSH client subcommands      | Medium-High | Emulate `ssh` CLI for one-off connections without YAML. Needs argument parsing, ephemeral config construction.                                                                |
| F4  | sshd: user switching        | High        | `setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows. Requires root/capabilities. Security-critical.                                                                   |
| F1  | Config hot-reload           | High        | File watcher, config diff, per-component context tree with independent cancellation. Currently all components share one root context with no restart capability.              |
| F11 | sshd: X11 forwarding        | High        | Xauth cookie handling, Unix socket, channel multiplexing. Low demand.                                                                                                         |

---

## Tier 5 — Release / packaging

| ID | Item                         | Notes                                                                     |
|----|------------------------------|---------------------------------------------------------------------------|
| R1 | Semantic versioning          | Tag `v0.1.0` or `v1.0.0`. Define stability commitment.                    |
| R2 | CHANGELOG.md                 | Start from current state.                                                 |
| R3 | Verify `go install` path     | End-to-end test: `go install github.com/mmdemirbas/mesh/cmd/mesh@latest`. |
| R4 | README: admin server docs    | Port file location, API endpoints, one curl example.                      |
| R5 | README: demo GIF             | Capture live dashboard in action.                                         |
| R6 | Homebrew formula             |                                                                           |
| R7 | Dockerfile                   |                                                                           |
| R8 | systemd unit + launchd plist |                                                                           |

---

## CI note

`.github/workflows/ci.yml` specifies Go 1.25, but README says Go 1.26+. Reconcile.

## Pre-existing flaky test

`TestAcceptAndForward_DialerErrorDropsConnection` occasionally fails with "connection reset by peer"
on `net.Dial` due to `SetLinger(0)` on accepted connections.

## My Notes

- clipsync: When we run multiple groups in the same LAN, all mesh instances will try to sync their
  clipboards with each other. We can define a group name or key to allow running multiple isolated
  groups.
- clipsync: use protobuf protocol for efficiency.
- filesync v1 implemented. Future enhancements: block-level delta, bandwidth throttling, TLS,
  selective sync, web UI improvements.
