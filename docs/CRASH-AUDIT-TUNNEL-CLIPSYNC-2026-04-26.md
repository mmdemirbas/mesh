# Crash audit — tunnel + clipsync — 2026-04-26

Scope: `internal/tunnel/`, `internal/clipsync/`. Sequel to the
filesync crash audit (same date). Looking for goroutines that
process peer-controlled or network-controlled data without panic
recovery, plus the usual nil-deref / map-race / channel-close
hazards.

## Critical

### C1 — shell I/O wait goroutine without panic recovery (Unix + Windows)

**Locations:**
- `internal/tunnel/shell_unix.go` · `handleSession`,
  the `cmdStart.Do` goroutine that waits on `cmd.Wait()` and
  sends `exit-signal` / `exit-status`.
- `internal/tunnel/shell_windows.go` · `handleSession`, the
  `cmdStart.Do` goroutine that waits on `cmd.Wait()` and
  sends `exit-status`.

`ch.SendRequest("exit-status", ...)` from `golang.org/x/crypto/ssh`
panics on a torn-down mux. Every SSH session that terminates —
clean exit, remote disconnect, keepalive timeout — runs through
this goroutine. With a persistent server-side SSH listener and a
24/7 mesh process, this is a matter of when, not if.

**Fix.** First defer in each goroutine:
`defer func() { if r := recover(); r != nil { slog.Error("...", "panic", r, "stack", string(debug.Stack())) } }()`,
matching the pattern used in `handleDirectTCPIP`,
`handleTCPIPForward`, `handleCancelTCPIPForward`.

**Confidence: 5/5.**

### C2 — Windows session goroutine without panic recovery

**Location:** `internal/tunnel/shell_windows.go` · `handleSession`,
the outer `go func() { defer closeCh(); for req := range reqs { ... } }()`
that processes peer-supplied SSH requests.

Reads peer-supplied payloads via `ssh.Unmarshal`, calls
`cmd.Process.Kill()`, writes the MOTD to the channel. Any panic
inside (ssh teardown, OS API quirk on a Windows reconnect) kills
the process.

**Fix.** Same defer pattern, immediately inside the goroutine
before the existing `defer closeCh()`.

**Confidence: 5/5.**

## High

### H1 — `handleTCPIPForward` accept-loop inner goroutine

**Location:** `internal/tunnel/tunnel.go` · `handleTCPIPForward`,
the per-connection goroutine inside the accept loop.

Calls `sshConn.OpenChannel("forwarded-tcpip", payload)` and
`netutil.CountedBiCopy(conn, ch, ...)`. The outer
`handleTCPIPForward` has its own recover but the inner per-conn
goroutine does not — the outer recover does not run on a panic
in a child goroutine.

**Fix.** Defer recover at the top of the inner goroutine.

**Confidence: 4/5.**

### H2 — `acceptAndForward` inner goroutines

**Location:** `internal/tunnel/tunnel.go` · `acceptAndForward`,
the per-connection goroutine that calls the dialer closure
(`client.Dial` or `net.DialTimeout`) and `netutil.CountedBiCopy`.

A client disconnect during an active forward races with
`client.Dial` against a torn `ssh.Client`. The ssh library has
been observed to nil-deref on torn mux state.

**Fix.** Defer recover at goroutine top.

**Confidence: 4/5.**

### H3 — Unix shell I/O copy goroutines

**Location:** `internal/tunnel/shell_unix.go` · `handleSession`
PTY copy pair: `go func() { _, _ = io.Copy(ch, ptm) }()` and
`go func() { _, _ = io.Copy(ptm, ch) }()`.

`io.Copy` into an ssh channel can panic on internal write-path
nil deref after channel teardown.

**Fix.** Defer recover on both copy goroutines.

**Confidence: 4/5.**

## Medium

### M1 — unguarded type assertion in `handleTCPIPForward`

**Location:** `internal/tunnel/tunnel.go` · `handleTCPIPForward`
— `ln.Addr().(*net.TCPAddr).Port`.

Currently safe (the listener is always TCP) but a future refactor
can break it; the outer `defer recover()` would catch the panic
silently. Hardening for clarity, not crash prevention.

**Fix.** Replace with the standard ok-check pattern.

**Confidence: 3/5.**

### M2 — `clipsync.Node` stores `context.Context` in a struct field

**Location:** `internal/clipsync/clipsync.go` · `Node.ctx`.

Violates Go convention: contexts must propagate through call
arguments, not via struct fields. Leads to silent silencing of
in-flight HTTP pushes after a graceful shutdown — postHTTP
sees an already-cancelled context on the next call. Not a
crash, but silent data loss in the clipboard sync path.

**Fix.** Pass `ctx` as a parameter to `postHTTP`, `pullHTTP`,
`registerPeerHTTP` from each call site.

**Confidence: 3/5.**

### M3 — `clipsync` fire-and-forget goroutines without panic recovery

**Locations:** `internal/clipsync/clipsync.go` —
`Broadcast` (`go n.postHTTP(addr, data)`),
`refreshHTTPRegistration` (`go n.registerPeerHTTP(addr)`),
`runUDPServer` (`go n.pullHTTP(peerAddr)` and
`go n.registerPeerHTTP(peerAddr)`).

I/O-heavy paths fire on every clipboard change. A library
panic kills the process.

**Fix.** Wrap each goroutine body in
`defer func() { if r := recover(); r != nil { slog.Error(...) } }()`.

**Confidence: 4/5.**

## Low

### L1 — agent-forwarding inner goroutines

**Location:** `internal/tunnel/shell_unix.go` agent-forwarding
path. Inactive for the user's current setup; defer the fix.

**Confidence: 3/5.**

## Summary

| Severity  | Count |
| --------- | ----- |
| Critical  | 2     |
| High      | 3     |
| Medium    | 3     |
| Low       | 1     |

**Top steady-state hazard: C1.** Every SSH session termination
runs through the unrecovered `cmd.Wait` + `SendRequest` goroutine.
With a persistent server-side listener and continuous uptime,
this is the most likely cause of a sudden process death.
