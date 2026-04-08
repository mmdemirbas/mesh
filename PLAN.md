# PLAN.md

Roadmap for mesh. Last verified on 2026-04-08.
Items ordered by priority within each tier.

---

## Tier 1 — Bugs & Design Issues

| ID  | Component | Item                                       | Notes |
|-----|-----------|--------------------------------------------|-------|
| B16 | state     | Components/metrics maps grow without bound | `components` map (mutex-protected) and `metrics` (`sync.Map`) accumulate entries with no eviction. Long-running instances leak memory. Recommended: TTL-based eviction (background goroutine, matches existing `evictOldAuthFailures` pattern in tunnel). Retention time may differ per component type. |
| B3  | clipsync  | Clipboard overwritten without user intent  | Rename `lan_discovery` (bool) to `lan_discovery_group` (list of strings). Using `group` alone would mislead users into thinking it applies to static peers. Disable dynamic discovery when list is empty/missing. Remove `allow_send_to` and `allow_receive` — complexity not worth the small gain. |

---

## Tier 2 — Security

| ID  | Component | Item                            | Notes |
|-----|-----------|---------------------------------|-------|
| S1  | clipsync  | No TLS for clipsync HTTP        | Switch to HTTPS only — no HTTP fallback, no extra config properties. Generate self-signed certs automatically if none provided. Make it work out of the box without user configuration. Security without complexity. |
| FS4 | filesync  | No TLS / auth for filesync HTTP | Same approach as S1 — share the auto-TLS implementation. HTTPS by default, zero-config. |

---

## Tier 3 — Performance & Optimization

| ID  | Component | Item                                    | Notes |
|-----|-----------|---------------------------------------- |-------|
| P1  | core      | Profile and optimize CPU + memory       | Full profiling pass. Identify hot paths and memory hogs. |
| P2  | cli       | Simplify CLI dashboard                  | Keep CLI dashboard but strip non-essential detail. See [CLI Dashboard Simplification](#cli-dashboard-simplification) below. |
| P3  | filesync  | Adaptive watch/scan                     | No new config properties. Implement a self-tuning heuristic that watches hot paths and polls the rest. See [Adaptive Watch/Scan Design](#adaptive-watchscan-design) below. |
| P4  | filesync  | Compress index + clipboard transfers    | Apply gzip compression to protobuf index pages. ~30 lines. 60-70% reduction on metadata-heavy payloads. No backward compatibility concern (no released clients). Just change the wire format. |
| P5  | filesync  | Index transfer format                   | Already protobuf (not YAML as initially assumed). Optimal for this use case. Combine with P4 — apply gzip on top of protobuf. |
| P6  | clipsync  | Compress clipboard data before sharing  | Apply gzip compression to all clipboard payloads including text. Small text still benefits from compression. Becomes more valuable when file/image support (N3) lands. |
| FS5 | filesync  | Outgoing delta index                    | `buildIndexExchange(folderID, 0)` always sends full index. Subsequent syncs could send only entries newer than last sent sequence. Requires per-peer sent-state tracking. |

---

## Tier 4 — Features

| ID  | Component | Item                                | Complexity  | Notes |
|-----|-----------|-------------------------------------|-------------|-------|
| N1  | filesync  | Metadata migration & e2e test       | Medium      | Collect `.stignore` files from: `~/code`, `~/Desktop/HUAWEI/{Desktop,Documents,Downloads,OneBox,Pictures}`, `~/.m2/repository`, `~/dev/mmdemirbas/mesh`, `~/.mesh/{conf,keys-lenovo,log}`, `~/code/spark-kit`. Refine into `ignore_patterns` blocks. Resolve existing conflicts. Test end to end across 3 computers. |
| N2  | build     | Remove vendor dir from VCS          | Trivial     | Remove `vendor/` from version control entirely — not needed. |
| N3  | clipsync  | File/image copy support             | Medium-High | Copy a file or directory on one computer, paste on another. Image clipboard content also in scope. Small files: transfer immediately via existing push mechanism. Large files: needs lazy-copy design (transfer only when user pastes). Lazy-copy feasibility on macOS and Windows is unknown — needs research. Two-phase approach: ship eager copy for small files first, design lazy copy separately. |
| N4  | admin     | Action history in web UI            | Medium      | Clipboard activity, file sync activity, past metrics. Partially started (clipboard activity tracking exists). |
| N5  | admin     | Show all logs in web UI             | Low         | Full log viewer in the web UI, not just recent ring buffer. |
| N6  | admin     | Tree-table layout for web dashboard | Medium      | Components listed in a flat table. A tree-table with collapsible nodes would better represent the hierarchy. |
| F13 | clipsync  | Payload size limit                  | Low         | Network side partially capped (`maxRequestBodySize`). Local clipboard read has no cap — large image can OOM sender. When cap is hit, produce a warning log and a dashboard indicator. Silent drops confuse users. |
| F7  | sshd      | Env var forwarding                  | Low         | `handleSession()` ignores `env` request type (RFC 4254 section 6.4). Add case: parse name/value, check against configurable allowlist (e.g. `AcceptEnv: ["LANG", "LC_*"]`), append to `cmd.Env`. |
| F9  | sshd      | Exit-signal reporting               | Low         | `shell_unix.go` doesn't check `Signaled()`. Process killed by signal reports exit code 0 instead of `exit-signal`. Add signal check + SSH signal name mapping (RFC 4254 section 6.10). Windows: always `exit-status`. |
| F10 | sshd      | Banner and MOTD                     | Low         | `ssh.ServerConfig` has no `BannerCallback`. Add config fields `banner` and `motd` (file paths). Pre-auth: banner callback. Post-auth: write MOTD to channel before shell spawn. Read files at startup. |
| F8  | sshd      | Signal forwarding                   | Medium      | `handleSession()` doesn't process `signal` request type (RFC 4254 section 6.9). Parse signal name, map to `syscall.Signal`, send to process group. `SysProcAttr.Setpgid = true` already set. Windows: no-op. |
| F12 | sshd      | Windows shell default               | Low         | Decided: use modern PowerShell (`pwsh.exe`). Document the requirement. |
| F2  | cli       | `mesh init` command                 | Medium      | Interactive config generator. Scaffolds starter YAML with common patterns. |
| F5  | sshd      | SFTP subsystem                      | Medium      | Add `subsystem` request handling for `sftp` name. Requires `github.com/pkg/sftp` (new dependency). Enables `scp`, `sftp`, `rsync` over mesh tunnels. Consider chroot/home-dir restriction. |
| F6  | sshd      | SSH agent forwarding                | Medium      | Handle `auth-agent-req@openssh.com`. Create temp Unix socket per session, forward over SSH, set `SSH_AUTH_SOCK`. Unix-only. Consider opt-in per listener. |
| F14 | gateway   | LLM API gateway                     | High        | Bidirectional translation between Anthropic and OpenAI API formats. See [GATEWAY_PLAN.md](GATEWAY_PLAN.md). Plan needs review and refinement before implementation. |

---

## Tier 5 — Testing

| ID | Component | Item                                                        | Notes |
|----|-----------|-------------------------------------------------------------|-------|
| T2 | tunnel    | Tunnel package coverage gaps                                | Remaining gaps: `runLocalForward`, `runRemoteForward`, `buildAuthMethods`, full SSH client lifecycle, multiplex mode, `ExitOnForwardFailure`. |
| T3 | all       | Integration tests: real SSH + clipsync                      | Full client-server SSH roundtrip. Clipsync push/pull between two in-process nodes. |
| T4 | proxy     | Non-CONNECT HTTP forward path untested                      | Exercises its own `dialer` call, `bufio.Reader` wrapping, and `BiCopy`. No test coverage. |
| T5 | tunnel    | Flaky `TestAcceptAndForward_DialerErrorDropsConnection`     | Occasionally fails with "connection reset by peer" on `net.Dial` due to `SetLinger(0)` on accepted connections. |

---

## Tier 6 — Release / Packaging

| ID | Component | Item                     | Notes |
|----|-----------|--------------------------|-------|
| R1 | release   | Semantic versioning      | Start with `v0.0.1`. |
| R2 | release   | CHANGELOG.md             | Start from current state. |
| R3 | release   | Verify `go install` path | End-to-end test: `go install github.com/mmdemirbas/mesh/cmd/mesh@latest`. |
| R4 | docs      | README: admin server docs | Port file location, API endpoints, one curl example. |

---

## Code Quality

Validated 2026-04-08. Items confirmed still present in source.

| ID  | Component | Item                                        | Notes |
|-----|-----------|---------------------------------------------|-------|
| CQ1 | tunnel   | Pre-compute `ak.Marshal()` at load time     | Called inside loop in `matchesAnyAuthorizedKey` (~line 101) on every auth attempt. Pre-compute once when loading authorized keys. |
| CQ2 | filesync | Stop debounce timer on context cancel        | `watcher.go` `run()` method (~line 138). Timer leaks if context cancelled during debounce. Add `timer.Stop()` in `ctx.Done()` case. |
| CQ3 | filesync | Remove unused `fw.mu sync.Mutex` field       | `watcher.go` (~line 36). Never locked or unlocked anywhere. Dead code. |
| CQ4 | proxy    | Replace `time.After` with `time.NewTimer`    | Accept-error backoff in `socks5.go:28` and `http.go:60`. Timer objects from `time.After` can't be stopped or GC'd. |
| CQ5 | proxy    | Remove redundant `remote.Close()` before defer | HTTP CONNECT handler (`http.go:142`). Defer already closes. |
| CQ6 | clipsync | Remove duplicate Windows check               | `clipsync.go:1575` vs `1584`. Second `runtime.GOOS == "windows"` check is unreachable. |
| CQ7 | cmd      | Call `signal.Stop` for signal channels        | `main.go:355` (SIGINT/SIGTERM) and `sigwinch_unix.go:14` (SIGWINCH). Signal handlers never cleaned up. |
| CQ8 | cmd      | Reduce allocations in `humanLogHandler.Handle` | `main.go:499, 508`. Two map allocations per log record. Consider `sync.Pool` or single-pass approach. |

---

## Parked

Deferred. Revisit later.

| ID  | Component | Item                         | Notes |
|-----|-----------|------------------------------|-------|
| F3  | cli       | SSH client subcommands       | Ad-hoc `mesh ssh user@host` without YAML config. Needs target parsing, terminal raw mode, SIGWINCH forwarding. |
| F4  | sshd      | User switching               | `setuid`/`setgid` on Unix, `CreateProcessAsUser` on Windows. Requires root. Security-critical. |
| F1  | core      | Config hot-reload            | File watcher, config diff, per-component context tree with independent cancellation. |
| F11 | sshd      | X11 forwarding               | Xauth cookie handling, Unix socket, channel multiplexing. Low demand. |
| R5  | docs      | README: demo GIF             | Capture live dashboard in action. |
| R6  | release   | Homebrew formula             | |
| R7  | release   | Dockerfile                   | Multi-stage build: golang builder + scratch runtime. Static binary needs only CA certs. |
| R8  | release   | systemd unit + launchd plist | systemd with security hardening. launchd with KeepAlive. Both need dedicated `_mesh` user. |

---

## CLI Dashboard Simplification

Current dashboard shows: header (node/version/pid/uptime/metrics), clipsync (listeners + peers), filesync (listeners + peers + folders), listeners (socks/http/ssh with children), connections (targets + forward sets + forwards), unmapped dynamic ports, and a log tail.

**Proposed changes:**

| Section | Action | Rationale |
|---------|--------|-----------|
| Header (node/version/pid/uptime/total metrics) | KEEP | Essential at-a-glance identification. |
| Config/log/UI paths | KEEP | Quick reference. |
| Clipsync (listeners + peer status) | KEEP | Lightweight, high diagnostic value. |
| Filesync peers | SIMPLIFY | Show one line per folder with status + file count + last sync. Remove per-peer address detail — move to web UI. |
| Listeners + active reverse tunnels | KEEP | Core network topology. |
| Connections (targets + forwards) | KEEP | Essential "what's connected to where" view. |
| Unmapped dynamic ports | REMOVE | Debug-only noise. Move to web UI diagnostics. |
| Per-row metrics | SIMPLIFY | Show tx/rx only on "producer" rows (listeners, active forwards). Remove from low-activity rows (peers, folder names, targets). |
| Log tail | KEEP | Limit to last ~20 lines. Full log in web UI. |

---

## Adaptive Watch/Scan Design

Goal: dynamically watch frequently-changing paths with fsnotify, poll the rest. No new config properties. Self-tuning.

**Change frequency tracking:** Maintain a `map[string]*FrequencyEntry` where each entry holds `{changeCount int, windowStart time.Time, lastDemotedAt time.Time}`. On each fsnotify event or scan-detected change, increment the counter for that directory. Every scan cycle, reset windows older than 5 minutes. A directory is "hot" if it has >5 changes per 5-minute window (compile-time constant, not user-facing).

**Promotion:** After each scan, compute hotness for all directories. If a directory is unmonitored but hot, and total watch count is below the soft limit (e.g., 3000, leaving headroom before the 4096 hard cap), add it to fsnotify. Track in `watchedPaths map[string]bool`.

**Demotion:** If a watched directory records 0 changes across 2 consecutive windows (~10 minutes), remove its fsnotify watch. Apply a cooldown of 10 minutes before re-promoting a demoted path to prevent thrashing. Track in `demotionCooldown map[string]time.Time`.

**Edge cases:**
- *Burst in new directory:* Detected on next scan cycle, promoted then. The burst itself is captured by the scan regardless.
- *Directory deletion:* fsnotify fires a Remove event; watcher removes the watch immediately. Stale cleanup (5-min interval) is the safety net.
- *Large initial scan:* First scan has no prior frequency data, so no promotions. Second scan begins adaptive behavior. Alternatively, seed hotness from initial change counts to prioritize large active subdirs immediately.
- *Watch limit pressure:* When near the soft limit, promote only the hottest paths (sort by change frequency, take top N that fit).
