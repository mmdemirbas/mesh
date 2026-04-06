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
| FS4 | filesync  | No TLS / auth for filesync HTTP | Same design as S1 — share the solution. Peer validation is IP-only. Any machine with the right IP gets full read/write access. |

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

## My Notes

- filesync - copy metadata from all 3 computers and generate filesync equivalents. Resolve existing
  conflicts. Sync ignores. Test end to end.
