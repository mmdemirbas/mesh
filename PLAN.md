# PLAN.md

Roadmap for mesh. Last updated 2026-04-08.
Items ordered by priority within each tier.

---

## Tier 1 — Security

| ID  | Component | Item                            | Notes |
|-----|-----------|---------------------------------|-------|
| S1  | clipsync  | No TLS for clipsync HTTP        | Switch to HTTPS only — no HTTP fallback, no extra config properties. Generate self-signed certs automatically if none provided. Make it work out of the box without user configuration. Security without complexity. |
| FS4 | filesync  | No TLS / auth for filesync HTTP | Same approach as S1 — share the auto-TLS implementation. HTTPS by default, zero-config. |

---

## Tier 2 — Performance & Optimization

| ID | Component | Item                              | Notes |
|----|-----------|-----------------------------------|-------|
| P1 | core      | Profile and optimize CPU + memory | Full profiling pass. Identify hot paths and memory hogs. |
| P2 | cli       | Simplify CLI dashboard            | Keep CLI dashboard but strip non-essential detail. See [CLI Dashboard Simplification](#cli-dashboard-simplification) below. |
| P3 | filesync  | Adaptive watch/scan               | No new config properties. Implement a self-tuning heuristic that watches hot paths and polls the rest. See [Adaptive Watch/Scan Design](#adaptive-watchscan-design) below. |

---

## Tier 3 — Features

| ID  | Component | Item                                | Complexity  | Notes |
|-----|-----------|-------------------------------------|-------------|-------|
| N3  | clipsync  | File/image copy support             | Medium-High | Copy a file or directory on one computer, paste on another. Image clipboard content also in scope. Small files: transfer immediately via existing push mechanism. Large files: needs lazy-copy design (transfer only when user pastes). Lazy-copy feasibility on macOS and Windows is unknown — needs research. Two-phase approach: ship eager copy for small files first, design lazy copy separately. |
| N4  | admin     | Action history in web UI            | Medium      | Clipboard activity, file sync activity, past metrics. Partially started (clipboard activity tracking exists). |
| N6  | admin     | Tree-table layout for web dashboard | Medium      | Components listed in a flat table. A tree-table with collapsible nodes would better represent the hierarchy. |
| F8  | sshd      | Signal forwarding                   | Medium      | `handleSession()` doesn't process `signal` request type (RFC 4254 section 6.9). Parse signal name, map to `syscall.Signal`, send to process group. `SysProcAttr.Setpgid = true` already set. Windows: no-op. |
| F2  | cli       | `mesh init` command                 | Medium      | Interactive config generator. Scaffolds starter YAML with common patterns. |
| F5  | sshd      | SFTP subsystem                      | Medium      | Add `subsystem` request handling for `sftp` name. Requires `github.com/pkg/sftp` (new dependency). Enables `scp`, `sftp`, `rsync` over mesh tunnels. Consider chroot/home-dir restriction. |
| F6  | sshd      | SSH agent forwarding                | Medium      | Handle `auth-agent-req@openssh.com`. Create temp Unix socket per session, forward over SSH, set `SSH_AUTH_SOCK`. Unix-only. Consider opt-in per listener. |
| F14 | gateway   | LLM API gateway                     | High        | Bidirectional translation between Anthropic and OpenAI API formats. See [GATEWAY_PLAN.md](GATEWAY_PLAN.md). Plan needs review and refinement before implementation. |

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
