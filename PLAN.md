# PLAN.md

Roadmap for mesh. Last updated 2026-04-09.
Batch update: 27 items completed across Tier 2 and Tier 3.
Organized by urgency. Tags in the Component column indicate the area.

---

## Tier 1 — Fix Now

Crashes, active CVEs, broken functionality, exploitable security issues.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| DEP1 | build     | Go 1.26.1 has 4 active CVEs                 | x509 auth bypass, x509 chain work, x509 policy validation, TLS 1.3 KeyUpdate DoS. All reached via gateway/filesync/clipsync code paths. Fixed in Go 1.26.2. |
| ~~E1~~ | ~~core~~ | ~~No `recover()` on server-side goroutines~~ | Done. Added `recover()` to `handleDirectTCPIP`, `handleSession`, `handleTCPIPForward`, `handleCancelTCPIPForward`, `handleSocks5`, `handleHTTPProxy`. |
| ~~E3~~ | ~~clipsync~~ | ~~`postHTTP` nil panic from malformed peer~~ | Done. Error from `http.NewRequestWithContext` now checked; malformed peer logged and skipped. |
| ~~C1~~ | ~~sshd~~ | ~~`keepalive@openssh.com` rejected~~       | Done. Added `case "keepalive@openssh.com"` → reply `true` in server global request handler. |
| ~~S2~~ | ~~clipsync~~ | ~~Unauthenticated HTTP endpoints~~       | Done. `canReceiveFrom` now validates against loopback, static peers, and UDP-discovered peers. |
| ~~S3~~ | ~~core~~ | ~~Gzip decompression bomb~~                | Done. `gzipDecode` in clipsync capped at `2 × maxClipboardPayload` (200 MB), filesync at `4 × maxIndexPayload` (40 MB). |
| ~~E2~~ | ~~gateway~~ | ~~`json.Marshal` error → nil body 200 OK~~ | Done. Marshal errors return 500 with error message in both `handleA2O` and `handleO2A`. |
| ~~W1~~ | ~~sshd~~ | ~~`password_command` hardcodes `sh -c`~~   | Done. Build-tagged `exec_unix.go` / `exec_windows.go` with `shellCommand()` helper. Windows prefers `pwsh.exe`, falls back to `cmd.exe`. |
| ~~W2~~ | ~~filesync~~ | ~~`filepath.Match` breaks on Windows~~ | Done. Replaced all `filepath.Match` with `path.Match` in `ignore.go`. |
| ~~W3~~ | ~~filesync~~ | ~~`os.Rename` over existing files fails on Windows~~ | Done. Added `renameReplace()` helper — tries rename, falls back to remove+rename. Used in `index.go` and `transfer.go`. |
| ~~S12~~ | ~~windows~~ | ~~`SO_REUSEADDR` allows port hijacking~~ | Done. Replaced with `SO_EXCLUSIVEADDRUSE` in `listen_windows.go`. |

---

## Tier 2 — Fix Soon

Security hardening, correctness, data integrity, protocol compliance.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| S1   | clipsync  | No TLS for clipsync HTTP                     | HTTPS only, auto-TLS with self-signed certs if none provided. Zero-config. |
| FS4  | filesync  | No TLS / auth for filesync HTTP              | Same auto-TLS approach as S1 — share the implementation. |
| ~~S4~~ | ~~admin~~ | ~~Admin server allows non-loopback bind~~ | Done. Config validation rejects non-loopback `admin_addr`. |
| ~~S5~~ | ~~sshd~~ | ~~No per-connection channel limit~~        | Done. `atomic.Int64` counter, reject above 1000 with `ssh.ResourceShortage`. |
| ~~C2~~ | ~~sshd~~ | ~~Default SSH user hardcoded to `root`~~   | Done. Defaults to `os/user.Current().Username`, falls back to `root`. |
| Q1   | core      | `gzipEncode`/`gzipDecode` duplicated        | Identical in clipsync and filesync. Extract to shared package. Prerequisite for consistent S3 fix and P5 pooling. |
| ~~U1~~ | ~~config~~ | ~~`Forward.Type` default not applied~~   | Done. `validateForwards` defaults empty Type to `"forward"`. |
| ~~P4~~ | ~~filesync~~ | ~~`io.ReadAll` up to 4 GB for delta response~~ | Done. Capped at 256 MB. |
| ~~S9~~ | ~~gateway~~ | ~~Upstream error body logged verbatim (64 MB)~~ | Done. Truncated to 512 bytes via `truncateBody()`. |
| ~~S6~~ | ~~filesync~~ | ~~`isLoopback` misses IPv4-mapped IPv6~~ | Done. Uses `net.ParseIP(ip).IsLoopback()`. |
| ~~S7~~ | ~~sshd~~ | ~~`GatewayPorts=clientspecified` allows `0.0.0.0`~~ | Done. Wildcard bind (0.0.0.0, ::) rejected for `clientspecified`; use `GatewayPorts=yes` to allow. |
| ~~E4~~ | ~~sshd~~ | ~~`ServerAliveInterval` Atoi error swallowed~~ | Done. Logs warning on invalid value. |
| ~~E5~~ | ~~sshd~~ | ~~`loadSigner`/`loadAuthorizedKeys` bare errors~~ | Done. Errors wrapped with file path context. |
| ~~DOC1~~ | ~~schema~~ | ~~JSON Schema missing `accept_env`, `banner`, `motd`~~ | Done. Added all three fields to Listener schema definition. |
| ~~W6~~ | ~~sshd~~ | ~~`sessionEnv` panics on UNC home directories~~ | Done. Uses `filepath.VolumeName()` instead of `home[:2]`. |
| C3   | gateway   | `thinking` blocks silently dropped           | Extended thinking content dropped in both translation directions. Increasingly used feature. Needs design decision. |
| C4   | gateway   | `response_format` silently dropped           | `json_object` mode parsed but dropped. Clients expecting guaranteed JSON get unstructured text. Needs design decision. |

---

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Robustness & Error Handling

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~E6~~ | ~~filesync~~ | ~~`rateLimitedReader` discards `WaitN` error~~ | Done. `WaitN` error propagated to callers. |
| E7   | config    | Validation errors lack node name context     | Multi-node configs produce ambiguous errors. `config.go:533,538`. |
| ~~E8~~ | ~~state~~ | ~~`SnapshotFull` atomicity claim is false~~ | Done. Comment corrected to reflect that components and metrics snapshots are not strictly atomic. |
| S8   | sshd      | `PermitOpen` bypass via alternate hostnames  | String comparison on unresolved `DestAddr`. Document limitation or restrict to IP-only. |
| S10  | config    | Writable config enables `password_command` injection | `warnInsecurePermissions` only warns. Consider hard reject. |
| S11  | clipsync  | UDP beacon port used for SSRF               | `msg.GetPort()` from unauthenticated beacon. Mitigated by fixing S2. |
| ~~D4~~ | ~~ops~~ | ~~Stale PID file detection~~               | Done. `upCmd` checks if PID is running; rejects if alive, removes if stale. |

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~P1~~ | ~~core~~ | ~~Profile and optimize CPU + memory~~      | Done. Commits `363f775`, `a27bbfa`, `3bb6b4d`. |
| P5   | core      | Pool `gzip.Writer`/`gzip.Reader`            | ~300 KB internal state allocated per request. `sync.Pool` with `Reset()`. |
| P6   | clipsync  | `proto.Marshal` re-serialization for size   | Pass original body size from HTTP handler instead. |
| P7   | filesync  | Maintain active file count                  | Four O(n) loops → maintain `activeCount` field. |
| P8   | filesync  | Merge temp cleanup into scan walk           | Eliminate redundant full tree traversal. |
| ~~P9~~ | ~~filesync~~ | ~~`hashFile` uses `fmt.Sprintf("%x")`~~ | Done. Replaced with `hex.EncodeToString`. |
| ~~P10~~ | ~~filesync~~ | ~~`hashEqual` manual loop~~           | Done. Replaced with `bytes.Equal`. |
| ~~P11~~ | ~~sshd~~ | ~~Parse `PermitOpen` once at startup~~    | Done. Pre-parsed into `permitOpenPolicy` struct with map lookups. |
| ~~P12~~ | ~~config~~ | ~~Normalize `GetOption` keys at load~~  | Done. `normalizeOptions` lowercases keys at load; `GetOption` does O(1) map hit. |
| P13  | cli       | Use `SnapshotFull()` in dashboard           | Two separate locks → one atomic call. |
| P3   | filesync  | Adaptive watch/scan                         | Self-tuning heuristic. See [design](#adaptive-watchscan-design) below. |

### UX & CLI

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| U2   | cli       | `mesh down` requires valid config file      | `down` only needs pidfiles. Scan `~/.mesh/run/mesh-*.pid` when no args given. |
| U3   | cli       | `mesh status -w` shows no metrics           | Always passes `nil` for `metricsMap`. Fetch from admin API. |
| U6   | cli       | `mesh help` omits filesync, gateway, admin  | Major feature discoverability gap. |
| U7   | cli       | Config not found gives raw OS error         | No guidance on search paths or examples. Poor first-run experience. |
| ~~U5~~ | ~~cli~~ | ~~`mesh status` exit code 3 undocumented~~ | Done. Exit codes documented in printUsage. |
| ~~U10~~ | ~~cli~~ | ~~`mesh down` exits 0 if nothing stopped~~ | Done. Exits 3 when no nodes were stopped. |
| U4   | cli       | Config errors lack YAML line numbers        | Array indices require manual counting in large files. |
| U8   | cli       | Dashboard log tail hard-coded to 10 lines   | Compute dynamically from viewport height. |
| U9   | admin     | Web UI fetches all 6 APIs every second      | Gate fetches by active tab. |
| P2   | cli       | Simplify CLI dashboard                      | See [design](#cli-dashboard-simplification) below. |

### Cross-Platform

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~W4~~ | ~~config~~ | ~~`warnInsecurePermissions` false positives on Windows~~ | Done. Build-tagged no-op on Windows (`perm_windows.go`). |
| ~~W5~~ | ~~config~~ | ~~`expandHome` only handles `~/`, not `~\`~~ | Done. Handles standalone `~`, `~/`, and `~\`. |
| W7   | clipsync  | `runtime.GOOS` instead of build tags        | 6 switch blocks. CLAUDE.md convention violated. Refactor to build-tagged files. |
| W8   | cli       | `checkPid`/`killPid` use `runtime.GOOS`    | Same violation. Move to build-tagged files. |

### Protocol Compatibility

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~C5~~ | ~~gateway~~ | ~~Static `msg_stream` as message ID~~ | Done. `generateMsgID()` produces unique `msg_<hex>` per stream. |
| C6   | sshd      | Server keepalive uses non-standard type     | `keepalive@golang.org` — non-Go clients may not reply. |
| C7   | sshd      | Public-key auth only on server side         | No password/keyboard-interactive server auth. Asymmetry may surprise users. |
| ~~C8~~ | ~~gateway~~ | ~~`n > 1` silently returns 1 choice~~ | Done. Returns error to client when n > 1. |
| ~~C9~~ | ~~gateway~~ | ~~Temperature silently clamped~~       | Done. Logs warning before clamping to 1.0. |

### Code Quality

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~Q2~~ | ~~sshd~~ | ~~Metrics reset duplicated 4 times~~      | Done. `Metrics.Reset()` replaces 4 identical blocks. |
| Q3   | sshd      | `runForwardSet` duplication                 | ~60 lines retry/handshake boilerplate. Extract `runConnectLoop`. |
| Q4   | gateway   | `handleA2O`/`handleO2A` duplication         | Extract shared `doUpstreamRequest`. |
| ~~Q5~~ | ~~sshd~~ | ~~Magic timing constants scattered~~      | Done. `defaultTCPKeepAlive`, `defaultHandshakeTimeout`, `defaultSSHClientTimeout`, `defaultServerAliveInterval`. |
| Q6   | core      | `activeNodes` registry duplicated           | Identical pattern in filesync and clipsync. Extract generic registry. |
| ~~Q7~~ | ~~sshd~~ | ~~`"keepalive"` sentinel not a constant~~ | Done. `keepaliveForwardSet` named constant. |
| ~~Q8~~ | ~~gateway~~ | ~~`mustMarshal` silently discards errors~~ | Done. Removed wrapper; all sites use explicit `json.Marshal` with error handling. |

### Documentation

| ID    | Component | Item                                         | Notes |
|-------|-----------|----------------------------------------------|-------|
| ~~DOC2~~ | ~~cli~~ | ~~`printUsage()` omits `help` command~~ | Done. Added to commands list. |
| ~~DOC3~~ | ~~cli~~ | ~~`printUsage()` tagline omits new features~~ | Done. Updated to mention clipsync, filesync, gateway. |
| ~~DOC4~~ | ~~cli~~ | ~~`mesh help` SSH options list incomplete~~ | Done. Added `StrictHostKeyChecking`. |
| ~~DOC5~~ | ~~readme~~ | ~~API endpoints section incomplete~~     | Done. Added /healthz, /api/filesync/*, /api/clipsync/activity. |
| ~~DOC6~~ | ~~readme~~ | ~~`task dist` output claim inaccurate~~  | Done. Fixed to show 4 actual binaries. |
| DOC7  | claude    | CLAUDE.md claims `CGO_ENABLED=0` always     | Darwin build omits it. |
| DOC8  | claude    | CLAUDE.md "never `runtime.GOOS`" violated   | 8 violations across clipsync and main. |
| ~~DOC9~~ | ~~schema~~ | ~~ForwardSet `options` description too narrow~~ | Done. Updated to list all 16 supported keys. |

### DevOps

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| D1   | ops       | Log rotation                                | Unbounded growth. SIGHUP + size-based rotation or external logrotate. |
| D2   | ops       | systemd / launchd service units             | No service management. Ship templates. |
| D3   | testing   | Tunnel package coverage at 34%              | Core forwarding functions at 0%. |
| D5   | testing   | Test parallelism                            | Zero `t.Parallel()` across 368 tests. |
| D6   | release   | Binary signing                              | No cosign/Sigstore. |
| ~~D7~~ | ~~admin~~ | ~~Health check endpoint~~                 | Done. `GET /healthz` returns 200 "ok". |
| D8   | ops       | `time.Sleep` in `downCmd` and tests         | Replace with channel-based sync. |
| D9   | testing   | Benchmark coverage                          | Only 3 benchmarks. No data-path benchmarks. |
| D10  | build     | darwin/arm64 dist allows CGO                | Align Taskfile with GoReleaser. |
| DEP2 | build     | `cmd/schema-gen` pulls CVE-affected dep     | Move to separate module to isolate `buger/jsonparser`. |
| DEP3 | build     | Charmbracelet TUI pulls 17 transitive modules | Consider raw terminal for viewport. |

---

## Tier 4 — Features

| ID   | Component | Item                                | Notes |
|------|-----------|-------------------------------------|-------|
| N3   | clipsync  | File/image copy support             | Copy a file or directory on one computer, paste on another. Image clipboard content also in scope. Small files: transfer immediately via existing push mechanism. Large files: needs lazy-copy design (transfer only when user pastes). Two-phase approach: ship eager copy for small files first, design lazy copy separately. |
| N4   | admin     | Action history in web UI            | Clipboard activity, file sync activity, past metrics. Partially started. |
| N6   | admin     | Tree-table layout for web dashboard | Collapsible nodes for component hierarchy. |
| F2   | cli       | `mesh init` command                 | Interactive config generator. Scaffolds starter YAML. |
| F5   | sshd      | SFTP subsystem                      | `subsystem` request handling. Requires `github.com/pkg/sftp`. Enables scp/sftp/rsync over mesh. |
| F6   | sshd      | SSH agent forwarding                | `auth-agent-req@openssh.com`. Temp Unix socket, `SSH_AUTH_SOCK`. Unix-only. |

---

## Parked

| ID   | Component | Item                         | Notes |
|------|-----------|------------------------------|-------|
| F3   | cli       | SSH client subcommands       | Ad-hoc `mesh ssh user@host`. Needs terminal raw mode, SIGWINCH. |
| F4   | sshd      | User switching               | `setuid`/`setgid` (Unix), `CreateProcessAsUser` (Windows). Root required. |
| F1   | core      | Config hot-reload            | File watcher, config diff, per-component context cancellation. |
| F11  | sshd      | X11 forwarding               | Xauth, Unix socket, channel multiplex. Low demand. |
| R5   | docs      | README: demo GIF             | |
| R6   | release   | Homebrew formula             | |
| R7   | release   | Dockerfile                   | Multi-stage build, scratch runtime. |

---

## Done

| ID   | Item                                | Notes |
|------|-------------------------------------|-------|
| ~~P1~~ | Profile and optimize CPU + memory | Regex → byte scanning, dashboard dirty check, log ring allocation, metrics caching, SSE JSON encoder reuse. Commits `363f775`, `a27bbfa`, `3bb6b4d`. |
| ~~F8~~ | SSH signal forwarding             | Unix: `syscall.Kill(-pid, sig)`. Windows: `Process.Kill()` for KILL/TERM/INT/HUP. |
| ~~R8~~ | systemd / launchd plist           | Promoted to D2. |
| ~~E1~~ | Server-side panic recovery        | `recover()` on all SSH channel, SOCKS5, HTTP proxy handler goroutines. |
| ~~E3~~ | `postHTTP` nil panic fix          | Error check on `http.NewRequestWithContext`; malformed peer logged and skipped. |
| ~~C1~~ | `keepalive@openssh.com` support   | Server replies `true` to OpenSSH client keepalives. |
| ~~S2~~ | Clipsync peer authentication      | `canReceiveFrom` validates against loopback, static peers, and discovered peers. |
| ~~S3~~ | Gzip decompression bomb defense   | `io.LimitReader` on gzip decoder in clipsync (200 MB) and filesync (40 MB). |
| ~~E2~~ | Gateway marshal error handling    | `json.Marshal` errors return 500 instead of empty 200. |
| ~~W1~~ | Cross-platform `password_command` | Build-tagged `shellCommand()`: `sh -c` on Unix, `pwsh.exe`/`cmd.exe` on Windows. |
| ~~W2~~ | Cross-platform ignore matching    | `filepath.Match` → `path.Match` for forward-slash consistency. |
| ~~W3~~ | Cross-platform atomic rename      | `renameReplace()` helper: remove-then-rename fallback for Windows. |
| ~~S12~~ | Windows port hijacking defense   | `SO_REUSEADDR` → `SO_EXCLUSIVEADDRUSE` on Windows. |
| ~~S4~~ | Admin loopback-only bind          | Config validation rejects non-loopback `admin_addr`. |
| ~~S5~~ | Per-connection channel limit       | `atomic.Int64` counter, reject above 1000 with `ssh.ResourceShortage`. |
| ~~C2~~ | SSH user defaults to OS username   | `os/user.Current().Username` instead of hardcoded `root`. |
| ~~U1~~ | Forward.Type defaults to forward   | `validateForwards` applies default when Type is empty. |
| ~~P4~~ | Delta response size cap            | Capped at 256 MB instead of 4 GB. |
| ~~S9~~ | Truncate upstream error logs       | Gateway upstream error body truncated to 512 bytes. |
| ~~S6~~ | IPv4-mapped IPv6 loopback          | `net.ParseIP(ip).IsLoopback()` in filesync. |
| ~~E4~~ | Keepalive interval parse warning   | Logs warning on non-integer `ServerAliveInterval`. |
| ~~E5~~ | SSH key error context              | `loadSigner`/`loadAuthorizedKeys` errors include file path. |
| ~~W6~~ | UNC home directory safety          | `filepath.VolumeName()` instead of `home[:2]` in Windows session env. |
| ~~P9~~ | `hashFile` hex encoding             | `hex.EncodeToString` instead of `fmt.Sprintf("%x")`. |
| ~~P10~~ | `hashEqual` compiler intrinsic    | `bytes.Equal` instead of manual byte loop. |
| ~~Q5~~ | Named timing constants              | `defaultTCPKeepAlive`, `defaultHandshakeTimeout`, `defaultSSHClientTimeout`, `defaultServerAliveInterval`. |
| ~~Q7~~ | Keepalive sentinel constant         | `keepaliveForwardSet` named constant replaces magic string coupling. |
| ~~Q8~~ | Gateway marshal error handling      | Removed `mustMarshal`; explicit `json.Marshal` with error propagation at all sites. |
| ~~E6~~ | Rate limiter WaitN error            | `rateLimitedReader.Read` propagates `WaitN` error to callers. |
| ~~E8~~ | SnapshotFull atomicity comment      | Corrected to reflect sync.Map.Range independence from mu.RLock. |
| ~~P11~~ | Pre-parse PermitOpen               | `permitOpenPolicy` struct with map-based O(1) matching at server startup. |
| ~~P12~~ | Normalize GetOption keys           | `normalizeOptions` lowercases at load; `GetOption` O(1) map lookup. |
| ~~W4~~ | Windows permission check            | Build-tagged no-op in `perm_windows.go`. |
| ~~W5~~ | expandHome backslash support        | Handles standalone `~`, `~/`, and `~\`. |
| ~~C5~~ | Unique stream message IDs           | `generateMsgID()` with crypto/rand hex per stream response. |
| ~~C8~~ | Reject n > 1                        | `translateOpenAIRequest` returns error for n > 1. |
| ~~C9~~ | Temperature clamp warning           | Logs warning before clamping to Anthropic max 1.0. |
| ~~S7~~ | Reject wildcard remote forward bind | `clientspecified` rejects 0.0.0.0 / ::; requires `GatewayPorts=yes`. |
| ~~D7~~ | Health check endpoint               | `GET /healthz` returns 200 "ok" on admin server. |
| ~~D4~~ | Stale PID file detection            | `upCmd` checks running PID, rejects if alive, removes if stale. |
| ~~U5~~ | Exit code documentation             | Documented 0/1/3 in `printUsage` Exit Codes section. |
| ~~U10~~ | `mesh down` exit code              | Exits 3 when no nodes were stopped. |
| ~~Q2~~ | Metrics.Reset() method              | Deduplicated 4 identical reset blocks in tunnel.go. |
| ~~DOC1~~ | JSON Schema completeness          | Added `accept_env`, `banner`, `motd` to Listener definition. |
| ~~DOC2~~ | `printUsage` help command         | Added `help` to commands list. |
| ~~DOC3~~ | `printUsage` tagline              | Updated to mention clipsync, filesync, gateway. |
| ~~DOC4~~ | SSH options completeness          | Added `StrictHostKeyChecking` to help text. |
| ~~DOC5~~ | README API endpoints              | Added /healthz, /api/filesync/*, /api/clipsync/activity. |
| ~~DOC6~~ | README dist claim                 | Fixed to show 4 actual binaries. |
| ~~DOC9~~ | Schema options description        | Updated to list all 16 supported SSH option keys. |

---

## Design: CLI Dashboard Simplification

| Section | Action | Rationale |
|---------|--------|-----------|
| Header (node/version/pid/uptime/total metrics) | KEEP | Essential at-a-glance identification. |
| Config/log/UI paths | KEEP | Quick reference. |
| Clipsync (listeners + peer status) | KEEP | Lightweight, high diagnostic value. |
| Filesync peers | SIMPLIFY | One line per folder with status + file count + last sync. Per-peer detail → web UI. |
| Listeners + active reverse tunnels | KEEP | Core network topology. |
| Connections (targets + forwards) | KEEP | Essential "what's connected to where" view. |
| Unmapped dynamic ports | REMOVE | Debug-only noise → web UI diagnostics. |
| Per-row metrics | SIMPLIFY | tx/rx only on "producer" rows (listeners, active forwards). |
| Log tail | KEEP | Limit to last ~20 lines. Full log in web UI. |

---

## Design: Adaptive Watch/Scan

Goal: dynamically watch frequently-changing paths with fsnotify, poll the rest. No new config properties. Self-tuning.

**Change frequency tracking:** `map[string]*FrequencyEntry` with `{changeCount, windowStart, lastDemotedAt}`. Increment on fsnotify event or scan-detected change. Reset windows older than 5 minutes. "Hot" = >5 changes per 5-minute window.

**Promotion:** After each scan, if a directory is hot and unwatched, and total watch count < soft limit (3000), add to fsnotify.

**Demotion:** 0 changes across 2 consecutive windows (~10 min) → remove watch. 10-min cooldown before re-promoting.

**Edge cases:**
- *Burst in new directory:* Detected on next scan, promoted then.
- *Directory deletion:* fsnotify Remove event; stale cleanup (5-min interval) as safety net.
- *Large initial scan:* No promotions on first scan. Second scan begins adaptive behavior.
- *Watch limit pressure:* Sort by frequency, promote top N that fit.

---

## Other Notes

- Auto load .env file from the current directory to load environment variables securely.
