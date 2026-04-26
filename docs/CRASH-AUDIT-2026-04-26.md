# Crash audit — 2026-04-26

Scope: all `internal/` and `cmd/mesh/`. Driven by the recurring
production crash (`perflog.go:328` nil deref of `fs.index` on a
disabled folder, fixed earlier in the same session). Looking for
additional hazards that fire in steady-state production.

Pre-fixed earlier today (excluded from this audit):
`perflog.go:328`, C1 `ClearPersisted`, H1 `fs.peers` RLock in
`syncFolder` peer-reset detect, M1 `foldersMu` plus `folderEntries`
helper, M2 `chmodFileRoot`.

## Critical

### CR-1 — point-lookup of `n.folders` without `foldersMu` in `buildIndexExchange` and `persistFolder`

**Locations:** `internal/filesync/filesync.go` ·
`Node.buildIndexExchange`, `Node.persistFolder`

Both functions call `fs, ok := n.folders[folderID]` outside of
any lock. `closeOneFolder` mutates the map under
`foldersMu.Lock()`. A sync goroutine that reads
`n.folders[folderID]` at the moment a `/reopen` or `/restore`
admin call deletes the entry hits a "concurrent map read and
map write" runtime panic. `buildIndexExchange` runs on every
30-second sync cycle per peer; the trigger is one admin call
during any sync window.

**Fix.** Both have a properly-locked accessor `n.findFolder(id)`
already (lines 3756–3766 of filesync.go). One-line substitution
at each call site.

**Confidence: 5/5.**

### CR-2 — range over `n.folders` without `foldersMu` in `runBackupSweep`

**Location:** `internal/filesync/filesync.go` · `runBackupSweep`

`runBackupSweep` holds `reopenLockMu`, which blocks reopen and
restore — but the live sync and scan goroutines never hold
`reopenLockMu`. Even when reopen IS held, `closeOneFolder`'s
`delete` runs under `foldersMu.Lock()`, and Go's map range is
not safe against concurrent deletes (regardless of which other
locks the caller holds). The 24-hour backup cadence makes this
rare, but it is real.

**Fix.** Use `n.folderEntries()` — same helper the other range
sites already use.

**Confidence: 5/5.**

### CR-3 — `GetFolderPath` and `folderCacheDirFor` read `n.folders` without `foldersMu`

**Location:** `internal/filesync/filesync.go` · `GetFolderPath`,
`folderCacheDirFor`

Both are called from admin HTTP handlers (backup list, conflict
list, folder status). Same race as CR-1: every page load in the
web UI fires these. `closeOneFolder` running under `foldersMu`
during any of these reads = panic.

**Fix.** Use `n.findFolder(folderID)`.

**Confidence: 5/5.**

## Important

### IMP-1 — `persistAllCtx` ranges `n.folders` without `foldersMu` (shutdown path)

**Location:** `internal/filesync/filesync.go` · `persistAllCtx`

Runs during shutdown after `ctx.Done()`. The admin server's
shutdown hook can race a concurrent `/reopen` call submitted
just before the cancel. Window is narrow but non-zero.

**Fix.** Use `n.folderEntries()`.

**Confidence: 4/5.**

### IMP-2 — `max_concurrent: 0` deadlocks every filesync goroutine

**Locations:** `internal/filesync/filesync.go` · `syncAllPeers`;
`internal/config/config.go` · `FilesyncCfg.UnmarshalYAML`

`UnmarshalYAML` sets `MaxConcurrent = 4`, then `value.Decode()`
overrides it with whatever the YAML says. `max_concurrent: 0`
resets it to zero. `syncAllPeers` creates
`make(chan struct{}, 0)` — unbuffered. Every sync goroutine
blocks on `sem <- struct{}{}` forever; nothing ever reads. All
filesync peer syncs hang permanently. The process stays alive,
filesync is silently dead.

**Fix.** In `UnmarshalYAML`, after `value.Decode()`, clamp:
`if c.MaxConcurrent <= 0 { c.MaxConcurrent = 4 }`. Optional
secondary: validate at config load time and emit an error
instead of silently fixing.

**Confidence: 4/5.**

## Low

### LOW-1 — bare type assertion in `handleTCPIPForward`

**Location:** `internal/tunnel/tunnel.go` ·
`handleTCPIPForward`

`actualPort := uint32(ln.Addr().(*net.TCPAddr).Port)` — no `ok`
check. Currently safe because the surrounding code already
guarantees a TCP listener; the function also has a
`defer recover()` so a hypothetical panic would be caught.
Worth hardening for readability and to remove a recovery dependency.

**Fix.** Two-line type-asserted branch with a logged failure
path.

**Confidence: 3/5.** Not a current crash risk.

## Summary

| Severity | Count |
| -------- | ----- |
| Critical | 3     |
| Important | 2    |
| Low      | 1     |

All three Critical findings are the same root cause: `n.folders`
must be accessed through `n.findFolder` or `n.folderEntries`
(both already exist and take `foldersMu`). The five missed call
sites — `buildIndexExchange`, `persistFolder`, `GetFolderPath`,
`folderCacheDirFor`, `runBackupSweep` — are mechanical
substitutions.

**Top steady-state hazard: CR-1.** `buildIndexExchange` runs
every 30 s per configured peer; `/api/filesync/folders/<id>/reopen`
during ANY of those windows is an immediate process crash.
