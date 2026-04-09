# PLAN.md

Roadmap for mesh. Last updated 2026-04-09.
Organized by urgency. Tags in the Component column indicate the area.

---

## Tier 1 — Fix Now

Crashes, active CVEs, broken functionality, exploitable security issues.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| DEP1 | build     | Go 1.26.1 has 4 active CVEs                 | x509 auth bypass, x509 chain work, x509 policy validation, TLS 1.3 KeyUpdate DoS. All reached via gateway/filesync/clipsync code paths. Fixed in Go 1.26.2. |
| E1   | core      | No `recover()` on server-side goroutines     | SSH channel handlers, SOCKS5/HTTP proxy handlers, filesync transfer goroutines — none have `recover()`. A single panic from malformed input crashes all subsystems. Only clipsync clipboard I/O has recovery. |
| E3   | clipsync  | `postHTTP` nil panic from malformed peer     | `http.NewRequestWithContext` error discarded. Malformed peer address from UDP discovery causes nil pointer panic. Concrete crash path with no `recover()`. `clipsync.go:376`. |
| C1   | sshd      | `keepalive@openssh.com` rejected             | Server replies `false` to keepalive requests. OpenSSH clients disconnect after `ServerAliveCountMax` failures. Add `case "keepalive@openssh.com"` → reply `true`. `tunnel.go:316-338`. |
| S2   | clipsync  | Unauthenticated HTTP endpoints               | `canReceiveFrom` and `canSendTo` are stubs returning `true`. Any LAN host can POST to `/sync` to inject clipboard content or GET `/clip` to exfiltrate it. Implement IP allowlist from `static_peers` + validated UDP-discovered peers, matching the filesync `validatePeer` pattern. |
| S3   | core      | Gzip decompression bomb                      | `gzipDecode` in clipsync and filesync calls `io.ReadAll` with no decompressed output limit. A 45-byte gzip bomb expands to gigabytes. Fix: `io.LimitReader` on the gzip reader (e.g., `4 × maxRequestBodySize`). |
| E2   | gateway   | `json.Marshal` error → nil body 200 OK       | `result, _ := json.Marshal(anthResp)` — if marshal fails, writes empty body with 200. Silent data loss for LLM clients. `gateway.go:191,282`. |
| W1   | sshd      | `password_command` hardcodes `sh -c`         | Breaks on Windows — `sh` not available. Use build-tagged helpers: `cmd.exe /C` or `powershell -Command` on Windows. `tunnel.go:499`. |
| W2   | filesync  | `filepath.Match` breaks on Windows           | Ignore patterns use `/`-normalized paths but `filepath.Match` uses `\` on Windows. Wrong files sync/excluded. Use `path.Match`. `ignore.go:98,110,119,142`. |
| W3   | filesync  | `os.Rename` over existing files fails on Windows | Unix atomically replaces; Windows fails "Access is denied." Breaks index persistence and file downloads. Use `os.Remove` + `os.Rename` or `MoveFileEx`. `index.go:79,116`, `transfer.go:113,237`. |
| S12  | windows   | `SO_REUSEADDR` allows port hijacking         | On Windows, `SO_REUSEADDR` lets another process steal the listening port. Use `SO_EXCLUSIVEADDRUSE`. `listen_windows.go:18`. |

---

## Tier 2 — Fix Soon

Security hardening, correctness, data integrity, protocol compliance.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| S1   | clipsync  | No TLS for clipsync HTTP                     | HTTPS only, auto-TLS with self-signed certs if none provided. Zero-config. |
| FS4  | filesync  | No TLS / auth for filesync HTTP              | Same auto-TLS approach as S1 — share the implementation. |
| S4   | admin     | Admin server allows non-loopback bind        | `admin_addr` not validated as loopback. `0.0.0.0:7777` exposes state, logs, pprof. Reject non-loopback at config validation. |
| S5   | sshd      | No per-connection channel limit              | Authenticated client can open unlimited channels → goroutine exhaustion. Add `atomic.Int64` counter, reject above 1000. |
| C2   | sshd      | Default SSH user hardcoded to `root`         | OpenSSH defaults to OS username. Causes unexpected root login attempts. `tunnel.go:536`. |
| Q1   | core      | `gzipEncode`/`gzipDecode` duplicated        | Identical in clipsync and filesync. Extract to shared package. Prerequisite for consistent S3 fix. |
| U1   | config    | `Forward.Type` default not applied           | Struct comment says "Defaults to forward" but validation rejects empty type. Most common config error for new users. |
| P4   | filesync  | `io.ReadAll` up to 4 GB for delta response   | `transfer.go` reads entire delta response into memory. Cap at 256 MB or stream directly. |
| S9   | gateway   | Upstream error body logged verbatim (64 MB)  | May contain API key fragments. Goes to log file and ring buffer. Truncate to 512 bytes. |
| S6   | filesync  | `isLoopback` misses IPv4-mapped IPv6         | String comparison misses `::ffff:127.0.0.1`. Use `net.ParseIP(ip).IsLoopback()`. |
| S7   | sshd      | `GatewayPorts=clientspecified` allows `0.0.0.0` | Reject wildcard bind addresses for remote forwards unless explicitly allowed. |
| E4   | sshd      | `ServerAliveInterval` Atoi error swallowed   | `"30s"` silently sets interval to 0, disabling keepalives. `tunnel.go:1572`. |
| E5   | sshd      | `loadSigner`/`loadAuthorizedKeys` bare errors | "ssh: no key found" with no file path context. Wrap with filename. `tunnel.go:1437-1448`. |
| DOC1 | schema    | JSON Schema missing `accept_env`, `banner`, `motd` | `additionalProperties: false` rejects valid configs in IDEs. |
| W6   | sshd      | `sessionEnv` panics on UNC home directories  | `home[:2]` assumes drive letter. Use `filepath.VolumeName()`. `shell_windows.go:39-40`. |
| C3   | gateway   | `thinking` blocks silently dropped           | Extended thinking content dropped in both translation directions. Increasingly used feature. |
| C4   | gateway   | `response_format` silently dropped           | `json_object` mode parsed but dropped. Clients expecting guaranteed JSON get unstructured text. |

---

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Robustness & Error Handling

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| E6   | filesync  | `rateLimitedReader` discards `WaitN` error   | Burst < buffer size silently bypasses rate limiting. `ratelimit.go:30`. |
| E7   | config    | Validation errors lack node name context     | Multi-node configs produce ambiguous errors. `config.go:533,538`. |
| E8   | state     | `SnapshotFull` atomicity claim is false      | `sync.Map.Range` independent of `mu.RLock`. Fix comment or move metrics into mutex. |
| S8   | sshd      | `PermitOpen` bypass via alternate hostnames  | String comparison on unresolved `DestAddr`. Document limitation or restrict to IP-only. |
| S10  | config    | Writable config enables `password_command` injection | `warnInsecurePermissions` only warns. Consider hard reject. |
| S11  | clipsync  | UDP beacon port used for SSRF               | `msg.GetPort()` from unauthenticated beacon. Mitigated by fixing S2. |
| D4   | ops       | Stale PID file detection                    | `upCmd` overwrites stale PID files from crashes. Check if running first. |

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| ~~P1~~ | ~~core~~ | ~~Profile and optimize CPU + memory~~      | Done. Commits `363f775`, `a27bbfa`, `3bb6b4d`. |
| P5   | core      | Pool `gzip.Writer`/`gzip.Reader`            | ~300 KB internal state allocated per request. `sync.Pool` with `Reset()`. |
| P6   | clipsync  | `proto.Marshal` re-serialization for size   | Pass original body size from HTTP handler instead. |
| P7   | filesync  | Maintain active file count                  | Four O(n) loops → maintain `activeCount` field. |
| P8   | filesync  | Merge temp cleanup into scan walk           | Eliminate redundant full tree traversal. |
| P9   | filesync  | `hashFile` uses `fmt.Sprintf("%x")`         | Replace with `hex.EncodeToString`. |
| P10  | filesync  | `hashEqual` manual loop                     | Replace with `bytes.Equal` (compiler intrinsic). |
| P11  | sshd      | Parse `PermitOpen` once at startup          | Pre-parse into `map[string]bool`. |
| P12  | config    | Normalize `GetOption` keys at load          | Lowercase once, map lookup O(1). |
| P13  | cli       | Use `SnapshotFull()` in dashboard           | Two separate locks → one atomic call. |
| P3   | filesync  | Adaptive watch/scan                         | Self-tuning heuristic. See [design](#adaptive-watchscan-design) below. |

### UX & CLI

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| U2   | cli       | `mesh down` requires valid config file      | `down` only needs pidfiles. Scan `~/.mesh/run/mesh-*.pid` when no args given. |
| U3   | cli       | `mesh status -w` shows no metrics           | Always passes `nil` for `metricsMap`. Fetch from admin API. |
| U6   | cli       | `mesh help` omits filesync, gateway, admin  | Major feature discoverability gap. |
| U7   | cli       | Config not found gives raw OS error         | No guidance on search paths or examples. Poor first-run experience. |
| U5   | cli       | `mesh status` exit code 3 undocumented      | Document exit codes in help text. |
| U10  | cli       | `mesh down` exits 0 if nothing stopped      | Track whether any node was stopped. |
| U4   | cli       | Config errors lack YAML line numbers        | Array indices require manual counting in large files. |
| U8   | cli       | Dashboard log tail hard-coded to 10 lines   | Compute dynamically from viewport height. |
| U9   | admin     | Web UI fetches all 6 APIs every second      | Gate fetches by active tab. |
| P2   | cli       | Simplify CLI dashboard                      | See [design](#cli-dashboard-simplification) below. |

### Cross-Platform

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| W4   | config    | `warnInsecurePermissions` false positives on Windows | `Mode().Perm()` returns synthetic `0666`. Skip on Windows. |
| W5   | config    | `expandHome` only handles `~/`, not `~\`    | Windows users write `~\`. Also check `~` + `os.PathSeparator`. |
| W7   | clipsync  | `runtime.GOOS` instead of build tags        | 6 switch blocks. CLAUDE.md convention violated. Refactor to build-tagged files. |
| W8   | cli       | `checkPid`/`killPid` use `runtime.GOOS`    | Same violation. Move to build-tagged files. |

### Protocol Compatibility

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| C5   | gateway   | Static `msg_stream` as message ID           | Should generate unique IDs. Breaks dedup/logging. |
| C6   | sshd      | Server keepalive uses non-standard type     | `keepalive@golang.org` — non-Go clients may not reply. |
| C7   | sshd      | Public-key auth only on server side         | No password/keyboard-interactive server auth. Asymmetry may surprise users. |
| C8   | gateway   | `n > 1` silently returns 1 choice           | No error returned to client. |
| C9   | gateway   | Temperature silently clamped                | > 1.0 clamped to 1.0 without indication. |

### Code Quality

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| Q2   | sshd      | Metrics reset duplicated 4 times            | Add `Metrics.Reset()` method. |
| Q3   | sshd      | `runForwardSet` duplication                 | ~60 lines retry/handshake boilerplate. Extract `runConnectLoop`. |
| Q4   | gateway   | `handleA2O`/`handleO2A` duplication         | Extract shared `doUpstreamRequest`. |
| Q5   | sshd      | Magic timing constants scattered            | Add `defaultTCPKeepAlive`, `defaultRetryInterval`. |
| Q6   | core      | `activeNodes` registry duplicated           | Identical pattern in filesync and clipsync. Extract generic registry. |
| Q7   | sshd      | `"keepalive"` sentinel not a constant       | Invisible coupling. Use named constant. |
| Q8   | gateway   | `mustMarshal` silently discards errors      | Remove wrapper, use explicit error handling. |

### Documentation

| ID    | Component | Item                                         | Notes |
|-------|-----------|----------------------------------------------|-------|
| DOC2  | cli       | `printUsage()` omits `help` command         | |
| DOC3  | cli       | `printUsage()` tagline omits new features   | Missing clipsync, filesync, gateway. |
| DOC4  | cli       | `mesh help` SSH options list incomplete     | Omits `StrictHostKeyChecking`. |
| DOC5  | readme    | API endpoints section incomplete            | 3 endpoints missing from README. |
| DOC6  | readme    | `task dist` output claim inaccurate         | Says 6 binaries, builds 4. |
| DOC7  | claude    | CLAUDE.md claims `CGO_ENABLED=0` always     | Darwin build omits it. |
| DOC8  | claude    | CLAUDE.md "never `runtime.GOOS`" violated   | 8 violations across clipsync and main. |
| DOC9  | schema    | ForwardSet `options` description too narrow  | Says 5 options, 16 actually work. |

### DevOps

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| D1   | ops       | Log rotation                                | Unbounded growth. SIGHUP + size-based rotation or external logrotate. |
| D2   | ops       | systemd / launchd service units             | No service management. Ship templates. |
| D3   | testing   | Tunnel package coverage at 34%              | Core forwarding functions at 0%. |
| D5   | testing   | Test parallelism                            | Zero `t.Parallel()` across 368 tests. |
| D6   | release   | Binary signing                              | No cosign/Sigstore. |
| D7   | admin     | Health check endpoint                       | Add `GET /healthz`. |
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
