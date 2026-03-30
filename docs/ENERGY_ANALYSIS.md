# Energy Efficiency Analysis — mesh

Static analysis of periodic wake-ups, subprocess spawning, and idle energy consumption.

## Energy Budget: Steady-State Wake-ups

When mesh is running idle (no data flowing), these are the periodic wake-ups:

| Source                   | Interval      | Wakes/min   | Subprocess?                      | Can eliminate?       |
|--------------------------|---------------|-------------|----------------------------------|----------------------|
| **Clipboard poll**       | 2s            | 30          | Yes (osascript/xclip/powershell) | Partially            |
| **Dashboard render**     | 1s            | 60          | No                               | Yes, when unchanged  |
| **UDP beacon broadcast** | 3s            | 20          | No                               | Yes, when no changes |
| **SSH server keepalive** | 5s (default)  | 12 per conn | No                               | Configurable already |
| **SSH client keepalive** | 15s (default) | 4 per conn  | No                               | Configurable already |
| **Peer cleanup**         | 10s           | 6           | No                               | Low value target     |
| **Rate limiter cleanup** | 5min          | 0.2         | No                               | Already fine         |

The top 3 are where nearly all idle energy goes.

## Detailed Findings

### 1. Clipboard Polling (HIGHEST IMPACT)

- **File:** `internal/clipsync/clipsync.go:437-498`
- **Interval:** 2 seconds (hardcoded constant `PollInterval` at line 32)
- **Work per tick:** Spawns OS subprocess (osascript on macOS, xclip/wl-paste on Linux, powershell
  on Windows) to read clipboard formats (text, HTML, RTF, image), then hashes and compares.
- **Windows optimization:** `getOSClipSeq()` provides a cheap sequence check that skips the
  subprocess if clipboard hasn't changed. macOS/Linux always spawn.
- **Subprocess cost:** ~1800 subprocess invocations/hour when idle with no clipboard changes (
  macOS/Linux).
- **Energy impact:** CRITICAL — continuous subprocess spawning is the single largest energy
  consumer.

### 2. Dashboard Rendering

- **File:** `cmd/mesh/dashboard.go:67`
- **Interval:** 1 second (hardcoded)
- **Work per tick:** `state.Snapshot()` full map copy, `state.SnapshotMetrics()` full metrics copy,
  string formatting with `fmt.Sprintf` (20-100+ calls per render), terminal write via
  `strings.Builder` + single `fmt.Print`.
- **Terminal I/O:** Well-optimized (in-place line overwrite, no full screen clear, single write
  call).
- **Energy impact:** HIGH — 60 wakes/min even when nothing has changed.

### 3. Status Watch Mode

- **File:** `cmd/mesh/main.go:540`
- **Interval:** 1 second (hardcoded)
- **Work per tick:** HTTP GET to admin server, JSON decode, status rendering, terminal repaint.
- **Energy impact:** HIGH — same as dashboard, only active when `mesh status -w` is running.

### 4. UDP Beacon Broadcast

- **File:** `internal/clipsync/clipsync.go:1348`
- **Interval:** 3 seconds (hardcoded)
- **Work per tick:** Refresh broadcast addresses (cached 30s), JSON marshal beacon, UDP send to all
  broadcast addresses.
- **Event path:** Also sends immediately via `notifyCh` on clipboard changes.
- **Energy impact:** MODERATE — 20 wakes/min, could be mostly event-driven.

### 5. SSH Keepalives

- **Server-side:** `tunnel/tunnel.go:1396` — default 5s interval, configurable via
  `ClientAliveInterval`.
- **Client-side:** `tunnel/tunnel.go:678` — default 15s interval, configurable via
  `ServerAliveInterval`.
- **Work per tick:** Single `SendRequest("keepalive@openssh.com", ...)` SSH request.
- **Energy impact:** MODERATE — per-connection, but already configurable.

### 6. Peer Discovery Cleanup

- **File:** `internal/clipsync/clipsync.go:1457`
- **Interval:** 10 seconds
- **Work per tick:** Lock peer map, iterate entries, delete stale (>15s).
- **Energy impact:** LOW — trivial work, reasonable interval.

### 7. Rate Limiter Cleanup

- **File:** `internal/tunnel/tunnel.go:71`
- **Interval:** 5 minutes
- **Work per tick:** Lock map, iterate entries, delete idle >10min.
- **Energy impact:** NEGLIGIBLE.

## Patterns Already Well-Optimized

- **Data transfer:** `BiCopy`/`CountedBiCopy` use blocking I/O with `sync.Pool` buffers — zero
  polling, event-driven.
- **Terminal output:** Single `strings.Builder` → single `fmt.Print` per frame — minimal syscalls.
- **Accept loops:** SOCKS5/HTTP proxy have 50ms backoff on errors. SSH server is rate-limited.
- **Connection retry:** Jittered backoff (base + 0-25% random), configurable interval, not a tight
  loop.
- **All tickers:** Properly stopped with `defer ticker.Stop()` — no ticker leaks.
- **Logging:** Conditional (only on state changes), not per-tick.

## Proposed Improvements

### High Value

#### 1. Configurable clipboard poll interval + raise default

The 2s poll spawns a subprocess every tick on macOS/Linux. Making the interval configurable lets
users trade latency for energy. Raising the default to 3-5s cuts subprocess spawning by 33-60%.

- Add `poll_interval` to clipsync config.
- Default: 3-5s (from current 2s).
- Power-conscious users can set 10-30s.

Note: True event-driven clipboard (e.g., `NSPasteboard.changeCount` via cgo, D-Bus signals on Linux)
would eliminate polling entirely but adds platform complexity and cgo dependency.

#### 2. Dashboard: skip render when state unchanged

Currently renders every 1s unconditionally. Compare current snapshot + metrics to previous frame;
skip terminal write if identical. Drops from 60 wakes/min to near-zero when idle, with no visible
difference.

#### 3. UDP beacon: increase periodic interval

Increase from 3s to 10-15s for liveness beacons. Rely on existing `notifyCh` event-driven path for
clipboard change notifications. Peers have 15s expiry, so 10s beacon is safe.

### Low Value (No Action Needed)

- **SSH keepalives** — already configurable, necessary for tunnel health.
- **Peer cleanup (10s)** — trivial cost, needed for correctness.
- **Rate limiter cleanup (5min)** — negligible.
- **Relay accept loop backoff** — missing 50ms backoff on errors (`proxy.go:85-90`), but only
  triggers on error path.
- **Shell PTY unpooled buffers** — rare, short-lived, not worth pooling.
- **`fmt.Sprintf` in dashboard** — at 1Hz this is noise.
