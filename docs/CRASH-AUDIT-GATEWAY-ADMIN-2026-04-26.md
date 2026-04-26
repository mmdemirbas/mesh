# Crash audit — gateway + admin + cmd/mesh — 2026-04-26

Scope: `internal/gateway/`, `cmd/mesh/admin*.go`, `cmd/mesh/main.go`,
`cmd/mesh/dashboard*.go`, `cmd/mesh/selfmon.go`. Sequel to the
filesync and tunnel/clipsync audits.

Note: there are uncommitted edits in
`internal/gateway/active_registry.go` and
`internal/gateway/audit.go` (UpstreamModel field rename WIP);
those compile errors are NOT in scope and not flagged here.

## Critical

### CR-1 — `probeLoop` long-running goroutine without panic recovery

**Location:** `internal/gateway/active_probe.go` · `probeLoop`

`probeLoop` is launched by `runActiveProbes` outside `net/http`'s
own connection-serving recover. It calls
`runOneProbe` → `doUpstreamRequest` → `classifyOutcome` →
`recordPassiveOutcome`. Any panic in that call chain — nil
deref against a future upstream config shape, type assertion
mismatch in a response classifier, runtime error in JSON parsing —
escapes the goroutine and kills the entire mesh process. Active
on every upstream where `health.active.enabled: true`.

**Fix.** First defer in `probeLoop`:
`defer nodeutil.RecoverPanic("gateway.probeLoop")` (or per-tick
recovery so the loop survives a single bad probe).

**Confidence: 5/5.**

### CR-2 — `cleanupLoop` panic kills process AND hangs shutdown

**Location:** `internal/gateway/audit.go` · `Recorder.cleanupLoop`

Launched as `go r.cleanupLoop()` from `NewRecorder`. Runs every
24 h, calls `cleanupOldFiles` → `os.ReadDir` /  `e.Info` /
`os.Remove`. A panic during cleanup kills the process. Worse:
`cleanupLoop` is responsible for closing `r.cleanupDone` via
`defer close(r.cleanupDone)`. If the panic occurs before that
defer runs, `Recorder.Close` blocks forever on `<-r.cleanupDone`.
A rare filesystem panic becomes a guaranteed shutdown hang
requiring SIGKILL.

**Fix.** Keep `defer close(r.cleanupDone)` at the top of
`cleanupLoop` so it always runs. Wrap the per-tick body
(`r.cleanupOldFiles()`) in a closure with its own
`defer nodeutil.RecoverPanic(...)` so a single bad tick does
not end the loop OR cause a shutdown hang.

**Confidence: 5/5.**

## High

### H1 — `/api/metrics` holds `mCacheMu` across slow I/O

**Location:** `cmd/mesh/admin.go` · metrics handler

`mCacheMu.Lock()` covers the entire compute body —
`state.Global.SnapshotFull()`, `filesync.GetFolderMetrics()`,
`filesync.SnapshotPeerSessionDropped()`, and `openFDCount()`.
On Linux `openFDCount` reads `/proc/self/fd`; on macOS uses
`getdirentries`. Under filesystem pressure / NFS stall / kernel
quirk these can block for seconds. While blocked, every
concurrent `/api/metrics` request (Prometheus scraper, admin
UI, `mesh status -w`) is serialized behind the mutex. If the
filesystem operation never returns, `/api/metrics` is
permanently unresponsive.

**Fix.** Compute the metrics body without the lock held, then
take the lock only to update the cache. Duplicate computation
during a 5 s expiry window is the standard cost of this pattern.

**Confidence: 4/5.**

## Low

### L1 — `startSelfMonitor` without panic recovery

**Location:** `cmd/mesh/selfmon.go` · `startSelfMonitor`

Permanent goroutine, calls only safe stdlib functions today.
Risk near-zero. Worth wrapping for consistency with the
project's stated long-running-goroutine recovery policy.

**Fix.** Defer recover at the top.

**Confidence: 3/5.**

## Summary

| Severity  | Count |
| --------- | ----- |
| Critical  | 2     |
| High      | 1     |
| Low       | 1     |

**Top crash hazard: CR-2.** The `cleanupLoop` failure mode is
worse than a normal goroutine panic — it kills the process AND
hangs subsequent graceful shutdowns through a never-closed
channel. Fix must keep `close(r.cleanupDone)` reachable on
every exit path.
