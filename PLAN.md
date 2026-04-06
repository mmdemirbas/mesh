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

| ID  | Item                                      | Location                           | Notes                                                                                                                                                                                                          |
|-----|-------------------------------------------|------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| S1  | No TLS for clipsync HTTP                  | `clipsync.go` `runHTTPServer`      | Needs design. Options: mTLS, pre-shared key over TLS, or opportunistic TLS with self-signed certs. Config schema needs `tls_cert`/`tls_key` fields. Must remain backward-compatible for localhost-only setups. |
| FS4 | No TLS / auth for filesync HTTP           | `filesync/protocol.go`            | Same design as S1 — share the solution. Peer validation is IP-only. Any machine with the right IP gets full read/write access. |

---

## Tier 3 — Testing

| ID | Item                                             | Location                         | Notes                                                                                                                                         |
|----|--------------------------------------------------|----------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| T2 | Tunnel package coverage gaps                     | `internal/tunnel/tunnel_test.go` | Remaining gaps: `runLocalForward`, `runRemoteForward`, `buildAuthMethods`, full SSH client lifecycle, multiplex mode, `ExitOnForwardFailure`. |
| T3 | Integration tests: real SSH handshake + clipsync | New test file(s)                 | Full client-server SSH roundtrip. Clipsync push/pull between two in-process nodes.                                                            |

---

## Tier 4 — Features

Ordered by estimated value and complexity. Each needs design before implementation.

| ID  | Item                             | Complexity  | Notes                                                                                                                                                                         |
|-----|----------------------------------|-------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| F14 | Clipsync: group isolation        | Low-Medium  | All LAN mesh instances currently discover and sync with each other. Add a `group` field to `ClipsyncCfg` and embed it in UDP beacons and HTTP `/discover` bodies. Peers with mismatched group keys ignore each other. Details below. |
| F15 | Clipsync: protobuf protocol      | Medium      | Replace JSON serialization in clipsync with protobuf for efficiency. 5 message types across 9 serialization sites. Details below. |
| F16 | Filesync: delta index exchange   | Medium      | Full index (up to 200K entries) is sent every 30s. Implement sequence-based filtering to send only entries changed since last sync. Details below. |
| F17 | Filesync: block-level delta sync | High        | Currently transfers entire files on any change. rsync-style rolling hash + block transfer would reduce bandwidth for edits to large files. Details below. |
| F18 | Filesync: bandwidth throttling   | Low-Medium  | No rate limiting on file transfers. Can saturate the link during large initial syncs. Details below. |
| F13 | Clipsync payload size limit      | Low         | Network side partially capped (`maxRequestBodySize`). Gap: local clipboard read has no cap. A large image can OOM the sender. Add a per-format size check in `pollClipboard`. |
| F7  | sshd: env var forwarding         | Low         | Handle `"env"` request type. Collect before `"shell"/"exec"`, apply configurable allowlist (`AcceptEnv`), append to `cmd.Env`.                                                |
| F9  | sshd: exit-signal reporting      | Low         | Check `WaitStatus.Signaled()`, map to SSH signal name, send `exit-signal` instead of `exit-status`.                                                                           |
| F10 | sshd: banner and MOTD            | Low         | `ssh.ServerConfig.BannerCallback` for pre-auth. Channel data write before shell for post-auth MOTD. Config: `banner` and `motd` fields.                                       |
| F8  | sshd: signal forwarding          | Medium      | Handle `"signal"` request type for non-PTY sessions. Map SSH signal names to `syscall.Signal`. Send to process group via `syscall.Kill(-pgid, sig)`.                          |
| F12 | Windows shell default            | Decision    | Current: `cmd.exe` via `COMSPEC`. PowerShell (`pwsh.exe`) is modern but not universally available. Decide and document. No ConPTY support yet.                                |
| F2  | `mesh init` command              | Medium      | Interactive config generator. Scaffolds a starter YAML with common patterns.                                                                                                  |
| F5  | sshd: SFTP subsystem             | Medium      | Handle `"subsystem"` request with `sftp` name. Requires `github.com/pkg/sftp` dependency (new). Enables `scp`, `sftp`, `rsync`.                                               |
| F6  | sshd: SSH agent forwarding       | Medium      | Handle `auth-agent-req@openssh.com`. Create per-session Unix socket, set `SSH_AUTH_SOCK`. Unix-only.                                                                          |
| F3  | SSH client subcommands           | Medium-High | Emulate `ssh` CLI for one-off connections without YAML. Needs argument parsing, ephemeral config construction.                                                                |
| F4  | sshd: user switching             | High        | `setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows. Requires root/capabilities. Security-critical.                                                                   |
| F1  | Config hot-reload                | High        | File watcher, config diff, per-component context tree with independent cancellation. Currently all components share one root context with no restart capability.              |
| F11 | sshd: X11 forwarding             | High        | Xauth cookie handling, Unix socket, channel multiplexing. Low demand.                                                                                                         |

---

### F14 — Clipsync group isolation (expanded)

**Problem:** All mesh instances with `lan_discovery: true` on the same network auto-discover and sync clipboards. Two teams on the same LAN get each other's clipboard data.

**Design:**

1. Add `group` string field to `ClipsyncCfg` (config.go). Default: empty string (= global group, backward-compatible).
2. Embed `group` in UDP beacons: add `"gk"` field to the beacon JSON struct (`clipsync.go:1488`).
3. In `runUDPServer` (`clipsync.go:1340`), after magic check and self-ID filter, reject beacons where `msg.GroupKey != n.config.Group`. Empty group matches empty group only.
4. Embed `group` in HTTP `/discover` body (`clipsync.go:1563`). In the `/discover` handler (`clipsync.go:418`), reject mismatches before `registerPeer()`.
5. Static peers bypass group check (explicitly configured = trusted).

**Changes:** `config.go` (add field + schema), `clipsync.go` (beacon struct, UDP recv, HTTP discover, 4 functions).

**Scope:** ~40 lines of logic + config schema + validation test.

---

### F15 — Clipsync protobuf protocol (expanded)

**Problem:** Clipsync uses JSON with base64-encoded binary blobs. For clipboard data with images or large files, this adds ~33% overhead from base64 + JSON structural overhead.

**Design:**

1. Define a `clipsync.proto` with 3 message types:
   - `SyncPayload` (replaces `Payload` struct: formats + files)
   - `Beacon` (replaces inline beacon struct: magic, id, port, hash, group)
   - `DiscoverRequest` (replaces inline discovery struct: id, port, hash, group)
2. Migrate HTTP endpoints first (`POST /sync`, `GET /clip`): `Content-Type: application/x-protobuf`. These carry the bulk of the data.
3. Migrate UDP beacons second. Beacons are small (~100 bytes) so benefit is minimal, but consistency matters.
4. Keep backward-compatible accept logic during rollout: try protobuf first, fall back to JSON if content-type header is missing.

**Affected sites:** 9 serialization points (3 `json.Marshal`, 1 `json.Unmarshal`, 5 `json.NewDecoder`). The `Payload`, `ClipFormat`, and `FileRef` structs become generated types.

**Risk:** Breaking change for mixed-version deployments. Mitigate with content-type negotiation in step 4.

**Scope:** New `.proto` file, ~200 lines of migration across `clipsync.go`, update tests.

---

### F16 — Filesync delta index exchange (expanded)

**Problem:** Every 30s, `syncFolder` calls `buildIndexExchange` which serializes the entire `FileIndex` (up to 200K entries) into a protobuf `IndexExchange` and sends it to each peer. The peer sends its full index back. For a folder with 100K files, this is ~10-20 MB per peer per cycle even when nothing changed.

**Design:**

1. Track per-peer `lastSentSequence` in `PeerState` (already has `LastSeenSequence`).
2. In `buildIndexExchange`, accept an optional `sinceSequence` parameter. Only include `FileEntry` items with `Sequence > sinceSequence`.
3. On the server side (`handleIndex`), the requesting peer sends its `since` field. The server filters its response the same way.
4. Add `since` field to the `IndexExchange` protobuf message (or re-add `IndexRequest` as a separate request type).
5. On first sync with a new peer (or after reconnect), `sinceSequence = 0` = full exchange. After that, only deltas.
6. Fallback: if the peer responds with `sequence < lastSentSequence` (peer restarted), fall back to full exchange.

**Changes:** `proto/filesync.proto` (add field), `filesync.go` (`buildIndexExchange`, `syncFolder`, `PeerState`), `protocol.go` (`handleIndex`).

**Scope:** ~80 lines of logic. The diff algorithm already handles partial remote indices correctly.

---

### F17 — Filesync block-level delta sync (expanded)

**Problem:** A 1-byte edit to a 1 GB file retransmits the entire file. For large files that change frequently (databases, logs, disk images), this wastes significant bandwidth and time.

**Design (rsync-style):**

1. **Chunking:** Divide files into fixed-size blocks (e.g., 128 KB). Compute a rolling hash (Adler-32 or xxHash) and a strong hash (SHA-256) per block.
2. **Block index:** Extend `FileInfo` protobuf with a repeated `BlockHash` field (weak hash + strong hash + offset).
3. **Transfer protocol:** When a file differs, the receiver sends its block hashes. The sender matches blocks and sends only: (a) literal data for new/changed blocks, (b) block references for matching blocks.
4. **Reassembly:** Receiver reconstructs the file from references + literal data, verifies full-file SHA-256.

**Complexity:** This is a significant feature. Consider using an existing Go library (`github.com/jbrekling/rsync` or similar) rather than implementing from scratch. New dependency required.

**Prerequisite:** F16 (delta index) should land first — it solves the common case (few files changed) with much less effort.

**Scope:** ~500-800 lines + new proto messages + new dependency.

---

### F18 — Filesync bandwidth throttling (expanded)

**Problem:** A large initial sync (e.g., 50 GB of files) saturates the network link, starving other mesh traffic (SSH, clipsync) and potentially other applications.

**Design:**

1. Add `max_bandwidth` field to `FilesyncCfg` (e.g., `"10MB/s"`, `"100Mbit/s"`).
2. Implement a token-bucket rate limiter shared across all download goroutines within a sync node.
3. Wrap the `io.Copy` in `downloadFile` (transfer.go) with a rate-limited reader: `io.Copy(f, rateLimiter.Reader(resp.Body))`.
4. Use `golang.org/x/time/rate` (already in Go extended stdlib) for the token bucket. No new dependency.
5. Upload throttling: wrap response writer in `handleFile` with a rate-limited writer.

**Changes:** `config.go` (add field + parse), `transfer.go` (wrap reader), `protocol.go` (wrap writer), `filesync.go` (initialize limiter).

**Scope:** ~60 lines of logic + config parsing. `golang.org/x/time/rate` is a single well-maintained package.

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

- filesync - copy metadata from all 3 computers and generate filesync equivalents. Resolve existing
  conflicts. Sync ignores. Test end to end.
