# PLAN.md

Roadmap for mesh. Last updated 2026-04-09.
Organized by urgency. Tags in the Component column indicate the area.

## Implementation Guidelines

When working on items from this plan, follow these rules:

**Commits:** Group changes into semantically coherent commits. One commit per logical change — don't mix unrelated fixes. Commit message: short first line describing *what*, body explaining *why*.

**Tests:** Every code change must have corresponding tests. New features: write failing tests first (TDD). Bug fixes: reproduce with a test before fixing. Modifications: verify both old and new behavior. Run `go test -count=1 -race ./...` before presenting.

**Documentation:** Update relevant docs when behavior changes: CLAUDE.md (architecture), README.md (user-facing), JSON schema (config validation), help text (CLI), PLAN.md (mark items done). Keep docs in sync with code.

**Quality gate:** Before marking an item done: Does it compile? Do all tests pass (with race detector)? Does it match existing code style? Are edge cases handled? Is it the simplest correct solution?

**Scope discipline:** Don't silently expand scope. If a fix reveals adjacent issues, note them separately — don't fix them in the same commit. If a task would benefit from a design discussion, stop and ask.

**Verification:** Run `go build ./...` after every change. Run package-level tests after editing a package. Run the full suite before committing. Never present code that fails to compile or test.

---

## Tier 1 — Fix Now

Crashes, active CVEs, broken functionality, exploitable security issues.

(All items completed.)

---

## Tier 2 — Fix Soon

Security hardening, correctness, data integrity, protocol compliance.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| S1   | clipsync  | No TLS for clipsync HTTP                     | HTTPS only, auto-TLS with self-signed certs if none provided. Zero-config. |
| FS4  | filesync  | No TLS / auth for filesync HTTP              | Same auto-TLS approach as S1 — share the implementation. |
| C3   | gateway   | `thinking` blocks silently dropped           | Extended thinking content dropped in both translation directions. Increasingly used feature. Needs design decision. |
| C4   | gateway   | `response_format` silently dropped           | `json_object` mode parsed but dropped. Clients expecting guaranteed JSON get unstructured text. Needs design decision. |

---

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Robustness & Error Handling

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| S8   | sshd      | `PermitOpen` bypass via alternate hostnames  | String comparison on unresolved `DestAddr`. Document limitation or restrict to IP-only. |
| S11  | clipsync  | UDP beacon port used for SSRF               | `msg.GetPort()` from unauthenticated beacon. Mitigated by fixing S2. |

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| P3   | filesync  | Adaptive watch/scan                         | Self-tuning heuristic. See [design](#adaptive-watchscan-design) below. |

### UX & CLI

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| U3   | cli       | `mesh status -w` shows no metrics           | Always passes `nil` for `metricsMap`. Fetch from admin API. |
| P2   | cli       | Simplify CLI dashboard                      | See [design](#cli-dashboard-simplification) below. |

### Cross-Platform

(All items completed.)

### Protocol Compatibility

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| C6   | sshd      | Server keepalive uses non-standard type     | `keepalive@golang.org` — non-Go clients may not reply. |
| C7   | sshd      | Public-key auth only on server side         | No password/keyboard-interactive server auth. Asymmetry may surprise users. |

### DevOps

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| D1   | ops       | Log rotation                                | Unbounded growth. SIGHUP + size-based rotation or external logrotate. |
| D2   | ops       | systemd / launchd service units             | No service management. Ship templates. |
| D3   | testing   | Tunnel package coverage at 34%              | Core forwarding functions at 0%. |
| D6   | release   | Binary signing                              | No cosign/Sigstore. |
| D8   | ops       | `time.Sleep` in `downCmd` and tests         | Replace with channel-based sync. |
| D10  | build     | darwin/arm64 dist allows CGO                | Align Taskfile with GoReleaser. |
| DEP2 | build     | `cmd/schema-gen` pulls CVE-affected dep     | Move to separate module to isolate `buger/jsonparser`. |
| DEP3 | build     | Charmbracelet TUI pulls 17 transitive modules | Consider raw terminal for viewport. |

---

## Tier 4 — Features

| ID   | Component | Item                                | Notes |
|------|-----------|-------------------------------------|-------|
| N3   | clipsync  | File/image copy support             | Copy a file or directory on one computer, paste on another. Image clipboard content also in scope. Small files: transfer immediately via existing push mechanism. Large files: needs lazy-copy design (transfer only when user pastes). Two-phase approach: ship eager copy for small files first, design lazy copy separately. |
| N4   | admin     | Action history in web UI            | Clipboard activity, file sync activity, past metrics. Partially started. |
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
| R6   | release   | Homebrew formula             | |
| R7   | release   | Dockerfile                   | Multi-stage build, scratch runtime. |

---

## Done

| ID   | Item                                | Notes |
|------|-------------------------------------|-------|
| P1   | Profile and optimize CPU + memory   | Regex → byte scanning, dashboard dirty check, log ring allocation, metrics caching, SSE JSON encoder reuse. Commits `363f775`, `a27bbfa`, `3bb6b4d`. |
| F8   | SSH signal forwarding               | Unix: `syscall.Kill(-pid, sig)`. Windows: `Process.Kill()` for KILL/TERM/INT/HUP. |
| R8   | systemd / launchd plist             | Promoted to D2. |
| E1   | Server-side panic recovery          | `recover()` on all SSH channel, SOCKS5, HTTP proxy handler goroutines. |
| E3   | `postHTTP` nil panic fix            | Error check on `http.NewRequestWithContext`; malformed peer logged and skipped. |
| C1   | `keepalive@openssh.com` support     | Server replies `true` to OpenSSH client keepalives. |
| S2   | Clipsync peer authentication        | `canReceiveFrom` validates against loopback, static peers, and discovered peers. |
| S3   | Gzip decompression bomb defense     | `io.LimitReader` on gzip decoder in clipsync (200 MB) and filesync (40 MB). |
| E2   | Gateway marshal error handling      | `json.Marshal` errors return 500 instead of empty 200. |
| W1   | Cross-platform `password_command`   | Build-tagged `shellCommand()`: `sh -c` on Unix, `pwsh.exe`/`cmd.exe` on Windows. |
| W2   | Cross-platform ignore matching      | `filepath.Match` → `path.Match` for forward-slash consistency. |
| W3   | Cross-platform atomic rename        | `renameReplace()` helper: remove-then-rename fallback for Windows. |
| S12  | Windows port hijacking defense      | `SO_REUSEADDR` → `SO_EXCLUSIVEADDRUSE` on Windows. |
| S4   | Admin loopback-only bind            | Config validation rejects non-loopback `admin_addr`. |
| S5   | Per-connection channel limit        | `atomic.Int64` counter, reject above 1000 with `ssh.ResourceShortage`. |
| C2   | SSH user defaults to OS username    | `os/user.Current().Username` instead of hardcoded `root`. |
| U1   | Forward.Type defaults to forward    | `validateForwards` applies default when Type is empty. |
| P4   | Delta response size cap             | Capped at 256 MB instead of 4 GB. |
| S9   | Truncate upstream error logs        | Gateway upstream error body truncated to 512 bytes. |
| S6   | IPv4-mapped IPv6 loopback           | `net.ParseIP(ip).IsLoopback()` in filesync. |
| E4   | Keepalive interval parse warning    | Logs warning on non-integer `ServerAliveInterval`. |
| E5   | SSH key error context               | `loadSigner`/`loadAuthorizedKeys` errors include file path. |
| W6   | UNC home directory safety           | `filepath.VolumeName()` instead of `home[:2]` in Windows session env. |
| S7   | Reject wildcard remote forward bind | `clientspecified` rejects 0.0.0.0 / ::; requires `GatewayPorts=yes`. |
| DOC1 | JSON Schema completeness            | Added `accept_env`, `banner`, `motd` to Listener definition. |
| P9   | `hashFile` hex encoding             | `hex.EncodeToString` instead of `fmt.Sprintf("%x")`. |
| P10  | `hashEqual` compiler intrinsic      | `bytes.Equal` instead of manual byte loop. |
| Q5   | Named timing constants              | `defaultTCPKeepAlive`, `defaultHandshakeTimeout`, `defaultSSHClientTimeout`, `defaultServerAliveInterval`. |
| Q7   | Keepalive sentinel constant         | `keepaliveForwardSet` named constant replaces magic string coupling. |
| Q8   | Gateway marshal error handling      | Removed `mustMarshal`; explicit `json.Marshal` with error propagation at all sites. |
| E6   | Rate limiter WaitN error            | `rateLimitedReader.Read` propagates `WaitN` error to callers. |
| E8   | SnapshotFull atomicity comment      | Corrected to reflect sync.Map.Range independence from mu.RLock. |
| P11  | Pre-parse PermitOpen                | `permitOpenPolicy` struct with map-based O(1) matching at server startup. |
| P12  | Normalize GetOption keys            | `normalizeOptions` lowercases at load; `GetOption` O(1) map lookup. |
| W4   | Windows permission check            | Build-tagged no-op in `perm_windows.go`. |
| W5   | expandHome backslash support        | Handles standalone `~`, `~/`, and `~\`. |
| C5   | Unique stream message IDs           | `generateMsgID()` with crypto/rand hex per stream response. |
| C8   | Reject n > 1                        | `translateOpenAIRequest` returns error for n > 1. |
| C9   | Temperature clamp warning           | Logs warning before clamping to Anthropic max 1.0. |
| D7   | Health check endpoint               | `GET /healthz` returns 200 "ok" on admin server. |
| D4   | Stale PID file detection            | `upCmd` checks running PID, rejects if alive, removes if stale. |
| U5   | Exit code documentation             | Documented 0/1/3 in `printUsage` Exit Codes section. |
| U10  | `mesh down` exit code               | Exits 3 when no nodes were stopped. |
| Q2   | Metrics.Reset() method              | Deduplicated 4 identical reset blocks in tunnel.go. |
| DOC2 | `printUsage` help command           | Added `help` to commands list. |
| DOC3 | `printUsage` tagline                | Updated to mention clipsync, filesync, gateway. |
| DOC4 | SSH options completeness            | Added `StrictHostKeyChecking` to help text. |
| DOC5 | README API endpoints                | Added /healthz, /api/filesync/*, /api/clipsync/activity. |
| DOC6 | README dist claim                   | Fixed to show 4 actual binaries. |
| DOC9 | Schema options description          | Updated to list all 16 supported SSH option keys. |
| DEP1 | Go 1.26.2 upgrade                   | Fixed 4 active CVEs (x509 auth bypass, chain, policy; TLS 1.3 KeyUpdate DoS). |
| DOC7 | CLAUDE.md CGO claim                 | Corrected to note darwin omits CGO_ENABLED. |
| DOC8 | CLAUDE.md platform-code claim       | Updated to reflect ongoing runtime.GOOS migration. |
| Q1   | Shared gzip package                 | Extracted internal/gziputil from clipsync and filesync duplicates. |
| P5   | Pooled gzip.Writer                  | sync.Pool in gziputil.Encode avoids ~300 KB allocation per request. |
| E7   | Node name in validation errors      | main.go wraps Validate() errors with node name for multi-node clarity. |
| S10  | Reject world-writable configs       | checkInsecurePermissions returns error instead of warning; LoadUnvalidated fails on 0022 perms. |
| W8   | Build-tagged checkPid/killPid       | Moved to pid_unix.go / pid_windows.go. Removed runtime.GOOS from main.go. |
| P6   | Avoid proto.Marshal for size        | processPayload receives body size from caller instead of re-marshaling. |
| P7   | Active file count from scan         | scan() returns count; activeCount() method replaces 4 inline loops. |
| P8   | Temp cleanup merged into scan       | Stale .mesh-tmp-* cleaned during walk instead of separate traversal. |
| P13  | SnapshotFull in dashboard           | Single call replaces separate Snapshot + SnapshotMetrics. |
| U8   | Dynamic log tail lines              | Computed from viewport height instead of hardcoded 10. |
| U2   | mesh down without config            | Discovers running nodes from ~/.mesh/run/mesh-*.pid. |
| U6   | Help text completeness              | Mentions filesync, gateway, admin web UI. |
| U7   | Config-not-found guidance           | Shows search paths and usage hint instead of raw OS error. |
| U4   | YAML line numbers in errors         | Parse errors include the line number of the failing node. |
| U9   | Web UI tab-gated fetches            | Only fetches APIs needed by the active tab. |
| N6   | Tree-table component grouping       | Dashboard groups components by type with collapsible headers. |
| Q6   | Generic activeNodes registry        | internal/nodeutil.Registry[T] replaces duplicate patterns in clipsync/filesync. |
| Q3   | Extract connectSSH helper           | Shared dial+keepalive+handshake for runForwardSet and runForwardSetForTarget. |
| Q4   | Extract doUpstreamRequest           | Shared upstream HTTP lifecycle for handleA2O and handleO2A. |
| D9   | Data-path benchmarks                | 16 benchmarks across 5 packages: BiCopy, scan, blockHash, state, gzip. |
| R5   | Demo tape for GIF generation        | VHS format demo.tape added; run `vhs demo.tape` to generate. |
| W7   | Clipsync build-tagged platform code | 6 runtime.GOOS switches → clipboard_darwin.go, clipboard_linux.go, clipboard_windows.go, clipboard_other.go. |
| D5   | Test parallelism                    | t.Parallel() added to 343 test functions and subtests across all packages. |

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

---

## Expansion Notes

### C3: `thinking` blocks silently dropped in gateway translation

**Goal:** Preserve or faithfully represent extended thinking content when translating between Anthropic and OpenAI formats.

**Approach:**
- In A2O (`translateAnthropicMessage`): for `thinking` blocks in outbound requests, prepend the thinking text as a `<parameter name="new_string">` XML-style wrapper in the assistant text content rather than dropping it, or drop with a one-time `slog.Warn` per request.
- In O2A (`translateAnthropicResponse`): `thinking` blocks appear in responses; surface them as an additional text block tagged `[thinking]` or drop with a log line.
- Add a `preserve_thinking` bool to `GatewayCfg` (default false = current behavior) so operators can opt in.
- In SSE streaming path, same logic applies per event delta.
- Add test cases covering thinking-only messages, mixed thinking+text, and thinking in tool_result context.

**Key decisions:** Whether to silently drop (current), log-and-drop, or attempt round-trip preservation. The OpenAI API has no native thinking type; any preservation is a lossy approximation. Decide before implementation.

**Risks/dependencies:** Anthropic extended thinking is gated behind `betas` header; the gateway must pass through `anthropic-beta` headers or add them when `preserve_thinking` is enabled.

**Effort:** S — the drop point is a single `case "thinking":` in two files; adding a log line is trivial. The opt-in preservation adds M work for the config field and streaming path.

---

### C4: `response_format` silently dropped in gateway translation

**Goal:** Honour `response_format: {type: "json_object"}` from OpenAI clients by instructing the upstream Anthropic model to emit JSON.

**Approach:**
- Parse `ResponseFormat` in `translateOpenAIRequest` (it is already decoded into `ChatCompletionRequest.ResponseFormat`).
- If `type == "json_object"`, append a system prompt suffix: `\n\nRespond with valid JSON only. Do not include any explanatory text outside of the JSON object.`
- If `type == "json_schema"` and a `schema` is present, also inject the schema into the system prompt.
- If `type == "text"`, no-op (current behavior is already correct).
- Log a `slog.Warn` for unknown `type` values.
- Add `response_format` to the dropped-fields comment in `openai.go` only for types we cannot handle.
- Add table-driven tests for each type variant.

**Key decisions:** Whether injection into the system prompt is the right mechanism vs. returning 400 for unsupported types. System prompt injection is imperfect (model may still deviate) but matches what most proxy gateways do.

**Risks/dependencies:** Anthropic models do not guarantee JSON output without a specific beta header (`computer-use-2024-10-22` is unrelated; no JSON mode beta exists). This is best-effort. Document the limitation in a comment.

**Effort:** S — parsing and system-prompt injection is straightforward. The `json_schema` variant is M if full schema injection is desired.

---

### S8: PermitOpen bypass via alternate hostnames

**Goal:** Prevent clients from bypassing `PermitOpen` restrictions by supplying a hostname that resolves to the same IP as a blocked target.

**Approach:**
- Option A (document-only): Add a code comment on `parsePermitOpen` and a user-facing note in `printHelp` explaining that matching is string-based on the `DestAddr` field from the SSH protocol, not on resolved IPs, and that operators should use IP addresses in `PermitOpen` if strict enforcement is needed.
- Option B (resolve at check time): In `handleDirectTCPIP`, after policy denies by name, attempt a DNS lookup of `DestAddr` and re-check each resolved IP against the policy. Log the lookup result. Cap lookup timeout at 2s via `context.WithTimeout`.
- Recommend Option A as the safer default (avoids TOCTOU, DNS lookup latency on every channel open, and DNS poisoning risk). Option B as an opt-in `PermitOpenResolve: yes` SSH option.

**Key decisions:** Whether resolution should happen at all inside the SSH server, or whether operators are responsible for using IPs in policy. The current pre-parsed `permitOpenPolicy` struct can be extended with an `allowResolve bool` field.

**Risks/dependencies:** DNS resolution inside the SSH server adds latency and a new external dependency. TOCTOU is inherent to any resolve-then-connect pattern. Option A has no risk.

**Effort:** Option A is XS (comment + help text). Option B is M (context-aware DNS lookup, tests for bypass scenario).

---

### D6: Binary signing (cosign/Sigstore)

**Goal:** Provide verifiable signatures for release binaries so users can confirm authenticity before running.

**Approach:**
- Add a `sign` task to `Taskfile.yml` that runs `cosign sign-blob` for each binary in `dist/` using a keyless OIDC flow tied to the GitHub Actions OIDC token.
- Store `.sig` and `.cert` files alongside each binary in the release assets.
- Add a `verify` task that runs `cosign verify-blob` against a given binary.
- Add a `checksums.txt` (SHA-256) generated by `sha256sum dist/*` and itself signed.
- Update `release` task in `Taskfile.yml` to call `sign` after `dist`.
- Document verification steps in `printHelp` output or a separate `INSTALL.md`.

**Key decisions:** Keyless (Sigstore OIDC, no stored private key) vs. key-based (GPG or cosign with a managed key). Keyless is simpler for CI but requires GitHub Actions; key-based allows local signing.

**Risks/dependencies:** cosign must be installed in the CI environment. Keyless flow requires GitHub Actions OIDC; local `task release` cannot use it without a separate key. Decision needed on local vs. CI-only signing.

**Effort:** M — Taskfile changes are small, but CI workflow setup and documentation add scope.

---

### D3: Tunnel package test coverage at 34%

**Goal:** Raise `internal/tunnel` coverage to at least 60%, covering core forwarding, auth, and server paths.

**Approach:**
- Run `go test -coverprofile=cover.out ./internal/tunnel/...` and `go tool cover -func=cover.out` to identify the zero-coverage functions.
- Prioritize: `handleDirectTCPIP`, `handleTCPIPForward`, `runForwardSet`, `buildAuthMethods`, `parsePermitOpen` (already has unit-testable shape).
- For `handleDirectTCPIP` and `handleTCPIPForward`: start an in-process SSH server with `net.Pipe()` and an SSH client; exercise permit-open allow/deny paths.
- For `runForwardSet`: test reconnect loop using a listener that closes after N accepts.
- For `buildAuthMethods`: table-driven tests with mock agent socket, key file, and `password_command`.
- Use `httptest.NewServer` pattern for any HTTP-adjacent paths.
- Add `t.Parallel()` once each test is self-contained.

**Key decisions:** Whether to use `net.Pipe()` (zero-network) or real TCP listeners (more realistic). `net.Pipe()` is preferred per project conventions (no `time.Sleep`, deterministic).

**Risks/dependencies:** `runForwardSet` has retry loops; tests must use context cancellation to bound execution time. Shell-dependent paths (`password_command`) need build-tag isolation.

**Effort:** L — each integration test requires setting up SSH server config, host keys, and auth. Targeting 60% is achievable in a focused session; 80%+ would require mocking OS-level calls.

---

### D2: systemd / launchd service units

**Goal:** Ship ready-to-use service unit templates so mesh can be managed by the OS process supervisor.

**Approach:**
- Create `configs/systemd/mesh@.service` — a templated unit where `%i` is the node name. Uses `ExecStart=/usr/local/bin/mesh -f /etc/mesh/mesh.yaml up %i`, `Restart=on-failure`, `RestartSec=5s`.
- Create `configs/launchd/io.github.mmdemirbas.mesh.plist` — a `launchd` plist with `RunAtLoad`, `KeepAlive`, and `StandardOutPath`/`StandardErrorPath` to `~/Library/Logs/mesh.log`.
- Add an `install:systemd` and `install:launchd` task to `Taskfile.yml` that copies the unit file and runs `systemctl daemon-reload` / `launchctl load`.
- Document `SIGHUP` behavior (currently no-op; see F1 hot-reload) so operators know a restart is needed for config changes.

**Key decisions:** Single-user vs. system-level service (affects unit file paths and privilege requirements). Ship both variants or just system-level.

**Risks/dependencies:** Hot-reload (F1) is parked; document that `systemctl restart mesh@node` is the config-change workflow until F1 lands. Launchd plist format differs between macOS versions; test on macOS 13+.

**Effort:** S — template files are straightforward. The install tasks add minor Taskfile work.

---

### D1: Log rotation

**Goal:** Prevent the mesh log file from growing unboundedly in long-running deployments.

**Approach:**
- Option A (external): Document that `logrotate` (Linux) and `newsyslog` (macOS) handle log rotation via SIGHUP or `copytruncate`. Add example configs to `configs/logrotate.d/mesh`. Mesh already reopens the log file on SIGHUP if the signal handler is extended to call `reopenLogFile()`.
- Option B (internal): Implement size-based rotation in the log setup code in `main.go`. On every write, check if file size exceeds a threshold (e.g. 100 MB). If so, rename `mesh.log` to `mesh.log.1` (removing any older `.1`), then open a new `mesh.log`. Use `sync.Mutex` around the rename+reopen. Keep at most 2 files.
- Option C: Use an existing `lumberjack` or similar package — rejected per project rule (no new dependencies without approval).
- Recommend Option A for now (zero code change, leverages OS tooling) plus a SIGHUP hook for Option B later.

**Key decisions:** Internal vs. external rotation. Internal is self-contained but adds complexity to the log setup. External requires operator configuration.

**Risks/dependencies:** SIGHUP handler currently only cancels the root context (shuts down the process). Extending it to reopen the log file without shutdown is non-trivial and must not race with in-flight log writes.

**Effort:** Option A is S (config templates + docs). Option B is M (atomic file rotation, mutex, tests).

---

### D10: darwin/arm64 dist allows CGO

**Goal:** Align the `dist` task with `CLAUDE.md`'s documented `CGO_ENABLED=0` for all targets.

**Approach:**
- The current `Taskfile.yml` omits `CGO_ENABLED` for `darwin/arm64`, relying on the host default (which is `1` on a Mac with Xcode). This produces a binary dynamically linked against system libraries.
- Add `CGO_ENABLED=0` to the `darwin/arm64` line in the `dist` task.
- Verify the resulting binary runs correctly: `file dist/mesh-darwin-arm64` should report `Mach-O 64-bit arm64 executable` without `dynamically linked`.
- Update the comment in `Taskfile.yml` to explain the rationale for all targets consistently.
- Also update `CLAUDE.md` `DOC7` claim to be accurate after this fix.

**Key decisions:** Whether CGO is actually needed for any darwin feature (e.g., clipboard access via Objective-C). The clipboard code uses `pbpaste`/`pbcopy` via `exec.Command`, so no CGO is required. Confirm by running the binary on a clean macOS VM without Xcode.

**Risks/dependencies:** If future clipboard or keychain integration requires CGO, this decision must be revisited. Flag this in a comment.

**Effort:** XS — a one-word change in `Taskfile.yml` plus a comment update.

---

### DEP2: cmd/schema-gen pulls CVE-affected dep

**Goal:** Isolate `cmd/schema-gen` and its `buger/jsonparser` dependency from the main module to prevent CVE exposure in the production binary.

**Approach:**
- Create `cmd/schema-gen/go.mod` as a separate Go module (e.g. `github.com/mmdemirbas/mesh/tools/schema-gen`).
- Move `cmd/schema-gen/main.go` imports to use a `replace` directive pointing to the parent module's `internal/config` package, or vendor the relevant structs.
- Update `Taskfile.yml` `schema` task to `cd cmd/schema-gen && go generate` or `go run .` within that directory.
- Add `.gitignore` entry for `cmd/schema-gen/go.sum` if desired, or commit it.
- After extraction, run `go mod tidy` on the main module and confirm `buger/jsonparser` is no longer in `go.sum`.

**Key decisions:** Whether `cmd/schema-gen` needs to import `internal/config` (yes, it does — `config.Config` is the schema root). A `replace` directive in the tool module pointing to `../..` handles this cleanly.

**Risks/dependencies:** The `replace` directive means the tool module is not independently publishable, but that is acceptable for a dev tool. CI must `cd cmd/schema-gen && go mod tidy` separately.

**Effort:** S — module extraction is mechanical. The `replace` directive pattern is well-understood.

---

### DEP3: Charmbracelet TUI pulls 17 transitive modules

**Goal:** Reduce the transitive dependency footprint introduced by `charmbracelet/bubbletea` and `charmbracelet/bubbles`.

**Approach:**
- Audit which Charmbracelet features are actually used: `bubbletea` is used for the `viewport` component in the web UI or dashboard. Identify the exact call sites.
- If only `viewport` (scroll buffer) is used, replace it with a hand-rolled ring-buffer + ANSI scroll approach using raw `golang.org/x/term` — which is already a direct dependency.
- If `lipgloss` styling is used, evaluate replacing it with the existing `cReset`/`cBold` ANSI vars already in the codebase.
- If Charmbracelet provides significant value (interactive widgets, input handling), keep it and accept the transitive cost, but document the decision.
- Run `go mod why github.com/charmbracelet/bubbletea` and `go mod graph` to map actual usage.

**Key decisions:** Whether the UX value of Charmbracelet justifies 17 extra modules in the binary. If the dashboard already uses raw ANSI (which it does — alternate screen, in-place overwrite), the TUI library may be redundant.

**Risks/dependencies:** Removing Charmbracelet without a replacement risks regressions in any interactive component that depends on its event loop. Full audit required before removal.

**Effort:** M for audit + replacement of one component. L if multiple components depend on the library.

---

### N3: File/image copy support for clipsync

**Goal:** Allow users to copy a file or image on one machine and paste it on another via the existing clipsync push mechanism.

**Approach:**
- Extend `SyncPayload` protobuf to include a `files` repeated field: `{name, size, data bytes}`.
- On the sender: when the clipboard contains file paths (macOS pasteboard `NSFilenamesPboardType`, Windows `CF_HDROP`), read each file up to `maxSyncFileSize` (50 MB, already defined) and populate the `files` field.
- On the receiver: write received files to a staging directory (`~/.mesh/clipsync-received/`), then set the clipboard to point to those paths.
- Image clipboard (`image/png`, `image/jpeg`): already flows through as raw bytes in the existing `formats` field. No change needed.
- Large files (>50 MB): log and skip for now; the PLAN.md note calls for a lazy-copy design as a follow-on.
- Gate behind `file_sync: true` config flag on `ClipsyncCfg` to avoid surprising users.

**Key decisions:** Where to stage received files (temp dir vs. fixed location). Fixed location (`~/.mesh/clipsync-received/`) is safer for paste operations. Cleanup policy (delete on next sync? TTL?).

**Risks/dependencies:** OS clipboard APIs for setting file paths vary significantly: macOS uses pasteboard item file URLs, Windows uses `CF_HDROP`, Linux uses `XDG_CURRENT_DESKTOP`-dependent methods. Build-tagged clipboard writers needed for each platform.

**Effort:** M for small-file path (protobuf extension + per-platform clipboard write). L if lazy-copy for large files is included.

---

### N4: Action history in web UI

**Goal:** Surface a chronological log of clipboard sync events and file sync events in the admin web UI.

**Approach:**
- Clipsync already maintains `activityHistory []*ClipActivity` (ring buffer of 20) exposed via `GET /api/clipsync/activity`. This is the foundation.
- Add a parallel `syncHistory` ring buffer to the filesync `Node` struct: `{time, folder, peer, direction, files int, bytes int64, err string}`. Populate it on each completed delta sync in the `syncWith` path.
- Expose via `GET /api/filesync/activity` (analogous to the clipsync endpoint).
- In the web UI, add an "Activity" tab (or section within Clipsync/Filesync tabs) that polls `/api/clipsync/activity` and `/api/filesync/activity` every 5 seconds and renders a combined timeline table.
- Gate polling by active tab (U9 fix) to avoid unnecessary requests.

**Key decisions:** Combined vs. per-component activity feed. Per-component is simpler and matches the existing API shape.

**Risks/dependencies:** The filesync `syncHistory` struct must be goroutine-safe (use a mutex-protected ring, same pattern as `logRing`). Size of the ring (20 entries matches clipsync; 50 may be better for filesync which is more active).

**Effort:** S for the filesync ring buffer and API endpoint. M for the web UI tab, depending on current UI structure.

---

### F2: `mesh init` command

**Goal:** Provide an interactive config generator that scaffolds a starter `mesh.yaml` for new users.

**Approach:**
- Add `case "init":` to the `main.go` switch. No config file required for this command.
- Use `bufio.Scanner` on `os.Stdin` for prompts (no new dependency; raw terminal not required).
- Ask in sequence: node name, role (client/server/both), listen address (for server), SSH key path, known_hosts path.
- Write a minimal YAML to the config path (default `~/.config/mesh/mesh.yaml` or path from `-f` flag).
- If the file already exists, ask before overwriting.
- Validate the generated config with `config.Load()` before writing.
- Print next steps: `mesh up`, `mesh status`.

**Key decisions:** Interactive prompts vs. flag-driven non-interactive mode. Flags (`--node`, `--role`, `--listen`) allow scripted use; prompts serve new users. Both can be supported with prompts falling back to flags.

**Risks/dependencies:** No new dependencies. The existing `config.Load` and `validate` functions provide validation. Windows line endings in the generated YAML must be `\n` (Go's `os.WriteFile` is platform-agnostic on content).

**Effort:** S — the command is self-contained and requires no library additions.

---

### F5: SFTP subsystem

**Goal:** Handle `subsystem sftp` requests in the SSH server, enabling `scp`, `sftp`, and `rsync` over mesh tunnels.

**Approach:**
- Add `github.com/pkg/sftp` as a dependency (requires approval per project rules).
- In `handleSession` (tunnel.go), detect `subsystem` channel requests with name `"sftp"`.
- Create an `sftp.NewRequestServer(channel, sftp.Handlers{...})` with read/write/list/stat handlers rooted at the authenticated user's home directory or a configurable `sftp_root` path on the listener.
- Respect `chroot_dir` if set (similar to OpenSSH's `ChrootDirectory`); use `filepath.Clean` + prefix check for path traversal safety.
- Add `sftp_enabled: true` and optional `sftp_root: /path` to `ListenerCfg`.
- Build-tag: SFTP handlers are pure Go; no platform-specific code needed.

**Key decisions:** Whether to expose the full filesystem or a chrooted subtree. Chrooted is safer and should be the default. Whether to support write operations or read-only initially.

**Risks/dependencies:** `github.com/pkg/sftp` adds a dependency — requires explicit approval. The library is mature and widely used. Without it, a from-scratch SFTP implementation is not feasible.

**Effort:** M — adding the dependency and wiring the subsystem handler is straightforward. Path traversal hardening and `sftp_root` config add a day of work.

---

### F6: SSH agent forwarding

**Goal:** Forward the client's SSH agent to remote sessions so users can use their local keys on remote hosts without copying private keys.

**Approach:**
- In `handleSession`, detect `auth-agent-req@openssh.com` channel request.
- Create a temp Unix socket at a path like `/tmp/mesh-agent-<sessionID>.sock`.
- Set `SSH_AUTH_SOCK` in the session environment (inject into `execEnv` before starting the shell/command).
- Start a goroutine that accepts connections on the socket and proxies them through a new SSH channel of type `auth-agent@openssh.com` back to the client.
- Clean up the socket in `defer` when the session ends.
- Unix-only: gate behind `//go:build !windows` build tag.

**Key decisions:** Socket path and cleanup. Using `os.MkdirTemp` for the socket directory is cleaner than a fixed `/tmp` path. Whether to support agent forwarding in `send-only` direction only or bidirectionally.

**Risks/dependencies:** The `auth-agent@openssh.com` channel type must be opened on the *existing* SSH connection, not a new one. The client must also support agent forwarding on their end. Unix socket support on macOS and Linux is consistent; not applicable to Windows.

**Effort:** M — the Unix socket proxy loop is the core complexity. Session lifecycle integration (env injection, cleanup) adds another half-day.

---

### F3: SSH client subcommands

**Goal:** Allow `mesh ssh [user@]host[:port]` as a convenience wrapper for interactive SSH sessions through mesh tunnels.

**Approach:**
- Add `case "ssh":` in `main.go`. Parse `[user@]host[:port]` from the remaining args.
- Put the terminal into raw mode using `golang.org/x/term` (already a dependency).
- Dial the SSH connection using the existing `buildAuthMethods` and `config.GetOption` logic from tunnel.go.
- Start a session with a PTY request (using `creack/pty` — already a dependency on Unix).
- Handle `SIGWINCH` on Unix via `signal.Notify` to send window resize requests.
- On exit, restore the terminal (`term.MakeRaw` → defer `term.Restore`).
- This is a client-side-only feature; no server changes needed.

**Key decisions:** Whether to support proxying through an intermediate mesh node (double-hop) or only direct SSH. Direct-only is simpler and covers the primary use case.

**Risks/dependencies:** `SIGWINCH` is Unix-only; Windows resizing uses a different mechanism (console API). Build-tag the SIGWINCH handler. The `creack/pty` dependency is already present.

**Effort:** M — raw terminal mode, PTY, and SIGWINCH handling are each small but must integrate cleanly. Windows support adds L effort.

---

### F4: User switching (setuid/setgid)

**Goal:** Allow the SSH server to start shells as the authenticated OS user rather than the process owner.

**Approach:**
- After successful SSH authentication, look up the OS user by the SSH username using `os/user.Lookup`.
- On Unix: call `syscall.Setgid(gid)` and `syscall.Setuid(uid)` in the child process after `fork` (or use `os/exec` with `SysProcAttr{Credential: &syscall.Credential{Uid, Gid}}`).
- On Windows: use `CreateProcessAsUser` with a user token obtained via `LogonUserW`. Build-tagged in `shell_windows.go`.
- Requires the mesh process to run as root (Unix) or as a privileged account (Windows).
- Add a `multi_user: true` config field to `ListenerCfg`. When false (default), skip user lookup and use process identity.
- Validate at startup: if `multi_user` is true and the process is not root, log a fatal error.

**Key decisions:** Whether to support PAM authentication as the credential check, or rely solely on SSH key auth with username-to-UID mapping. SSH key auth with username mapping is simpler and avoids PAM dependency.

**Risks/dependencies:** `syscall.Setuid` affects the entire process on some OS/thread combinations. Use `os/exec` with `SysProcAttr` to scope privilege drop to the child process only. This is a significant security surface; extensive testing on each platform is required.

**Effort:** L — platform-specific credential handling, security testing, and edge cases (supplementary groups, PAM, home directory) make this a multi-week effort.

---

### F1: Config hot-reload

**Goal:** Allow mesh to reload its configuration without restarting, applying changes to connections, forwards, and listeners while preserving existing sessions.

**Approach:**
- Watch the config file with `fsnotify` (already a dependency).
- On change event, call `config.Load` and `validate`. If validation fails, log the error and keep the current config.
- Diff the new config against the old: identify added, removed, and modified nodes/listeners/connections.
- For removed components: cancel their context (each component already respects context cancellation).
- For added components: start new goroutines.
- For modified components: cancel and restart — full restart of that component's goroutine tree.
- `state.Global` handles component lifecycle; the reload just cancels/re-creates context subtrees.
- Gate behind a `hot_reload: true` top-level config flag or always-on.

**Key decisions:** Granularity of reload — per-component cancel+restart vs. full process restart with `exec.Command(os.Args[0], ...)`. Per-component is more elegant but harder to implement correctly. Full-process restart via `syscall.Exec` is simpler and avoids state leaks.

**Risks/dependencies:** Each component must cleanly release resources on context cancel before new instances start. This assumes goroutine exit paths are already correct (they are, per CLAUDE.md conventions). The diff logic is the hardest part: config structs lack unique stable IDs across reload.

**Effort:** L — the fsnotify watcher is trivial, but the diff/cancel/restart lifecycle is complex. Full-process `exec` approach is M.

---

### F11: X11 forwarding

**Goal:** Forward X11 connections from remote clients through mesh SSH sessions to the local X server.

**Approach:**
- In `handleSession`, detect `x11-req` channel request. Parse the request payload: `single_connection`, `auth_protocol`, `auth_cookie`, `screen_number`.
- Generate a fake `DISPLAY` value (`:10` + session index) and a Unix socket or TCP listener on the server side.
- When the client opens an `x11` channel, proxy it to the `DISPLAY` socket on the server host (typically `localhost:6010`).
- Set `DISPLAY` in the session environment.
- Implement xauth cookie replacement: replace the client's `MIT-MAGIC-COOKIE-1` with a locally generated one, and add it via `xauth add`.

**Key decisions:** Whether to support the Unix socket path (`/tmp/.X11-unix/X10`) or TCP (`localhost:6010`) or both. TCP is simpler and sufficient for most use cases.

**Risks/dependencies:** Requires `xauth` binary on the server. The xauth cookie replacement is security-critical (must not forward the client's raw cookie to untrusted displays). Low demand per PLAN.md note; implement only if user demand increases.

**Effort:** L — protocol parsing, socket proxy, and xauth integration are each non-trivial. Cross-platform complexity (no X11 on macOS natively, no X11 on Windows) limits the audience.

---

### R6: Homebrew formula

**Goal:** Allow macOS users to install mesh via `brew install mesh`.

**Approach:**
- Create a Homebrew tap repository: `github.com/mmdemirbas/homebrew-tap`.
- Write a `Formula/mesh.rb` that downloads the `darwin-arm64` and `darwin-amd64` release binaries (or builds from source using `go build`).
- Use the `bottle do` block with pre-built binaries for fast install; source build as fallback.
- Add a `test do` block: `assert_match "mesh", shell_output("#{bin}/mesh -version")`.
- Update the `release` task in `Taskfile.yml` to compute the SHA-256 of each binary and update the formula automatically via `sed` or a small Go script.
- Document installation: `brew tap mmdemirbas/tap && brew install mesh`.

**Key decisions:** Tap-based (simpler, no Homebrew core submission requirements) vs. official Homebrew core (requires popularity threshold and review). Start with a tap.

**Risks/dependencies:** The tap repository must be maintained separately. Formula updates must be automated as part of the release process, otherwise it will fall out of sync.

**Effort:** S for the initial formula. M if automated formula updates are included in the release pipeline.

---

### R7: Dockerfile

**Goal:** Provide a minimal container image for running mesh in Docker/Kubernetes environments.

**Approach:**
- Multi-stage `Dockerfile`: builder stage uses `golang:1.26-alpine` with `CGO_ENABLED=0`; runtime stage uses `scratch` (no OS, smallest possible image).
- Copy only the compiled binary and any required CA certificates (`ca-certificates` from alpine) into the scratch image.
- `ENTRYPOINT ["/mesh"]` with `CMD ["up"]`.
- Expose no ports in the Dockerfile (ports are runtime config); document with `# EXPOSE 22 7777` as a comment.
- Add a `.dockerignore` excluding `dist/`, `build/`, `*.md`, and test files.
- Add a `docker` task to `Taskfile.yml`: `docker build -t mesh:{{.VERSION}} .`.
- Provide a `configs/docker-compose.yaml` example mounting a config file.

**Key decisions:** Whether to use `scratch` (smallest, no shell for debugging) or `alpine` (adds 5 MB but enables `sh` exec for debugging). Ship `scratch` as default, document `alpine` variant as a comment.

**Risks/dependencies:** The `scratch` image cannot run `password_command` via `sh -c` since there is no shell. Operators using `password_command` must use a non-scratch base or pass secrets via environment variables instead.

**Effort:** S — the Dockerfile is straightforward for a static binary. The docker-compose example and documentation add minor work.
