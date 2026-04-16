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

**Regression tests:** Every bug fix must include a test that reproduces the exact failure. Every feature must include tests that pin its behavior. When a constant becomes configurable, test both the default and custom values — and test that derived values (like body size limits) scale correctly. When a struct gains new fields, verify all test helpers that construct it are updated.

**Post-batch review:** After completing a batch of features or fixes, run a code review across all modified files before moving on. The B1-B6 incident showed that focused feature work introduced 6 bugs in adjacent code: a blocking call in a request loop, a double-close without sync.Once, a compile-time constant that should have been runtime, a wrong relative path, a missing SIGWINCH handler, and an incorrect defer order. All were found only by a systematic post-batch review — none would have been caught by feature-level testing alone.

**Move done items to DONE.md:** When a roadmap item is finished, remove it from PLAN.md (the table row and any detail section) and append it to DONE.md under the matching tier heading. Keeps PLAN.md focused on what is still in flight; DONE.md is the audit trail.

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
| [B8–B11, B4–B7, SR1, SR2, SR12] | filesync | Filesync production readiness (Phase 0a/0b) | All completed — see DONE.md. |
---

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Robustness & Error Handling

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| [S11](#s11-udp-beacon-port-used-for-ssrf) | clipsync | UDP beacon port used for SSRF | `msg.GetPort()` from unauthenticated beacon. Largely mitigated by S2. |

### S11: UDP beacon port used for SSRF

**Problem:** Clipsync UDP beacons carry a `port` field (`msg.GetPort()`) that tells the receiver which HTTP port the sender listens on. This field is taken from an unauthenticated UDP broadcast — any device on the LAN can send a forged beacon with an arbitrary port.

When a node receives a beacon, it uses `msg.GetPort()` to construct the peer's HTTP address (`{sender_ip}:{untrusted_port}`), then makes HTTP requests to that address (`/discover`, `/clip`). An attacker can set `port` to an internal service port (e.g., 80, 8080, 9090), causing the mesh node to make HTTP requests to arbitrary ports on the sender's IP — a classic SSRF vector.

**Current mitigation (S2):** The `canReceiveFrom` function validates that incoming HTTP requests come from known, trusted peers (loopback, static peers, or previously discovered peers). This means the attacker's forged beacon creates outbound requests from the victim node, but the victim's own HTTP server rejects requests from unknown sources. The SSRF risk is that the victim node acts as an HTTP client toward the attacker-controlled port, not that the attacker accesses the victim's data.

**Residual risk:** A LAN attacker can still cause the mesh node to probe arbitrary ports on the attacker's own IP (or any spoofed source IP in the beacon). This leaks connection success/failure information and could interact with services that trigger actions on incoming HTTP requests.

**Fix:** Validate `msg.GetPort()` against a reasonable range (e.g., 1024-65535) and optionally reject beacons whose `port` differs from the UDP source port by more than a configured threshold. Or: ignore the beacon `port` field entirely and always use the configured clipsync port.

**Effort:** S — a single validation check on `msg.GetPort()` in the beacon handler.

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| [P3](#p3-adaptive-watchscan) | filesync | Adaptive watch/scan | Self-tuning heuristic. Design below. |
| P14  | tunnel | Parallel target probing (Happy Eyeballs) | Stagger-start all targets concurrently in `probeTarget` instead of sequential. First to connect wins, respecting target order preference within a short tie-break window. Reduces worst-case probe time from sum(timeouts) to max(stagger, fastest_target). Nice-to-have. |

#### P3: Adaptive Watch/Scan

Goal: dynamically watch frequently-changing paths with fsnotify, poll the rest. No new config properties. Self-tuning.

**Change frequency tracking:** `map[string]*FrequencyEntry` with `{changeCount, windowStart, lastDemotedAt}`. Increment on fsnotify event or scan-detected change. Reset windows older than 5 minutes. "Hot" = >5 changes per 5-minute window.

**Promotion:** After each scan, if a directory is hot and unwatched, and total watch count < soft limit (3000), add to fsnotify.

**Demotion:** 0 changes across 2 consecutive windows (~10 min) → remove watch. 10-min cooldown before re-promoting.

**Edge cases:**
- *Burst in new directory:* Detected on next scan, promoted then.
- *Directory deletion:* fsnotify Remove event; stale cleanup (5-min interval) as safety net.
- *Large initial scan:* No promotions on first scan. Second scan begins adaptive behavior.
- *Watch limit pressure:* Sort by frequency, promote top N that fit.

### UX & CLI

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| U3   | cli       | `mesh status -w` shows no metrics           | Always passes `nil` for `metricsMap`. Fetch from admin API. |
| [P2](#p2-cli-dashboard-simplification) | cli | Simplify CLI dashboard | Log tail removed + body dirty-check landed. Design below. |

#### P2: CLI Dashboard Simplification

| Section | Action | Rationale |
|---------|--------|-----------|
| Header (node/version/pid/uptime/total metrics) | KEEP | Essential at-a-glance identification. |
| Config/log/UI paths | KEEP | Quick reference. |
| Clipsync (listeners + peer status) | KEEP | Lightweight, high diagnostic value. |
| Filesync peers | SIMPLIFY | One line per folder with status + file count + last sync. Per-peer detail → web UI. |
| Listeners + active reverse tunnels | KEEP | Core network topology. |
| Connections (targets + forwards) | KEEP | Essential "what's connected to where" view. |
| Unmapped dynamic ports | REMOVE | Debug-only noise → web UI diagnostics. |
| Per-row metrics | SIMPLIFIED | tx/rx shown only on producer rows (listeners and individual forwards). Connection-name, forward-set, and dynamic sub-rows are clean. Bytes still roll up into the sshd listener row and the grand total. |
| Log tail | REMOVED | Caused layout shifts and flicker as new lines arrived. Full log stays in the admin UI and on disk. |

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
| [D1](#d1-log-rotation) | ops | Log rotation | Unbounded growth. SIGHUP + size-based rotation or external logrotate. |
| [D2](#d2-systemd--launchd-service-units) | ops | systemd / launchd service units | No service management. Ship templates. |
| [D3](#d3-tunnel-package-test-coverage-at-34) | testing | Tunnel package coverage at 34% | Core forwarding functions at 0%. |
| [D6](#d6-binary-signing-cosignsigstore) | release | Binary signing | No cosign/Sigstore. |
| D8   | ops       | `time.Sleep` in `downCmd` and tests         | Replace with channel-based sync. |
| [D10](#d10-darwinarm64-dist-allows-cgo) | build | darwin/arm64 dist allows CGO | Align Taskfile with GoReleaser. |
| [D11](#d11-modernize-waitgroupgo-migration) | refactor | `sync.WaitGroup.Go` migration (22 sites)    | Mechanical migration deferred — needs per-site review for panic recovery and error propagation. |
| [D12](#d12-modernize-unused-parameters) | refactor | Unused parameters in tunnel.go (2 sites)    | `unusedparams` analyzer flagged `id` and `t0`. Removing them changes function signatures; must check call sites. |
| [D13](#d13-gateway-audit-historical-file-browsing) | gateway | Audit UI shows only newest jsonl file       | `/api/gateway/audit` and the UI tab read only the most-recent file in each gateway dir. After rollover, older rows become invisible to the UI. |
| [D14](#d14-gateway-audit-spillover-to-disk) | gateway | Audit body buffer is in-memory (64 MB cap)  | Single in-flight passthrough response is bounded; multi-tenant or pathological cases should spill to a temp file. |
| [D15](#d15-gateway-passthrough-e2e-coverage) | gateway | No e2e scenario for passthrough             | S4 covers a2o/o2a translation only. An a2a passthrough scenario against the stub would catch wire-level regressions unit tests can't. |
| [D16](#d16-flaky-tcp-test-on-macos) | testing | `TestAcceptAndForward_DialerErrorDropsConnection` flakes on macOS | `SetLinger(0)` on the accepted side can RST during the TCP handshake. Current 3-attempt retry isn't always enough; failed in CI on `504ac77` and `c831f27`. |

#### D1: Log rotation

**Goal:** Prevent the mesh log file from growing unboundedly in long-running deployments.

**Approach:**
- Option A (external): Document that `logrotate` (Linux) and `newsyslog` (macOS) handle log rotation via SIGHUP or `copytruncate`. Add example configs to `configs/logrotate.d/mesh`. Mesh already reopens the log file on SIGHUP if the signal handler is extended to call `reopenLogFile()`.
- Option B (internal): Implement size-based rotation in the log setup code in `main.go`. On every write, check if file size exceeds a threshold (e.g. 100 MB). If so, rename `mesh.log` to `mesh.log.1` (removing any older `.1`), then open a new `mesh.log`. Use `sync.Mutex` around the rename+reopen. Keep at most 2 files.
- Option C: Use an existing `lumberjack` or similar package — rejected per project rule (no new dependencies without approval).
- Recommend Option A for now (zero code change, leverages OS tooling) plus a SIGHUP hook for Option B later.

**Key decisions:** Internal vs. external rotation. Internal is self-contained but adds complexity to the log setup. External requires operator configuration.

**Risks/dependencies:** SIGHUP handler currently only cancels the root context (shuts down the process). Extending it to reopen the log file without shutdown is non-trivial and must not race with in-flight log writes.

**Effort:** Option A is S (config templates + docs). Option B is M (atomic file rotation, mutex, tests).

#### D2: systemd / launchd service units

**Goal:** Ship ready-to-use service unit templates so mesh can be managed by the OS process supervisor.

**Approach:**
- Create `configs/systemd/mesh@.service` — a templated unit where `%i` is the node name. Uses `ExecStart=/usr/local/bin/mesh -f /etc/mesh/mesh.yaml up %i`, `Restart=on-failure`, `RestartSec=5s`.
- Create `configs/launchd/io.github.mmdemirbas.mesh.plist` — a `launchd` plist with `RunAtLoad`, `KeepAlive`, and `StandardOutPath`/`StandardErrorPath` to `~/Library/Logs/mesh.log`.
- Add an `install:systemd` and `install:launchd` task to `Taskfile.yml` that copies the unit file and runs `systemctl daemon-reload` / `launchctl load`.
- Document `SIGHUP` behavior (currently no-op; see F1 hot-reload) so operators know a restart is needed for config changes.

**Key decisions:** Single-user vs. system-level service (affects unit file paths and privilege requirements). Ship both variants or just system-level.

**Risks/dependencies:** Hot-reload (F1) is parked; document that `systemctl restart mesh@node` is the config-change workflow until F1 lands. Launchd plist format differs between macOS versions; test on macOS 13+.

**Effort:** S — template files are straightforward. The install tasks add minor Taskfile work.

#### D3: Tunnel package test coverage at 34%

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

#### D6: Binary signing (cosign/Sigstore)

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

#### D10: darwin/arm64 dist allows CGO

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

#### D11: modernize WaitGroup.Go migration

**Goal:** Replace 22 `wg.Add(1) + go func(){ defer wg.Done(); ... }()` patterns with `wg.Go(func(){ ... })` (Go 1.25+).

**Approach:**
- Sites: `internal/filesync/filesync.go` (5), `internal/proxy/proxy.go` (2), `internal/tunnel/tunnel.go` (12), `cmd/mesh/main.go` (5). Run `~/go/bin/modernize -waitgroup ./...` for the current list.
- For each site: confirm the goroutine body has no early `wg.Done()` (e.g. inside a select), no panic recovery wrapping that depends on the explicit `defer wg.Done`, and no error-channel pattern that needs ordering against `wg.Done`.
- `wg.Go` runs the function synchronously up to the first await — same as `Add`+`go func` in practice, but the rewrite is mechanical only if the body is straightforward.

**Key decisions:** Whether to use `modernize -waitgroup -fix` and rely on tests (969 across 14 packages) to catch any semantic break. The risk is that subtle differences in defer ordering or panic-recovery behavior slip through without a failing test.

**Effort:** S–M — `modernize -fix` runs in a second; per-site review is the time sink.

#### D12: modernize unused parameters

**Goal:** Remove unused parameters `id` (tunnel.go:521) and `t0` (tunnel.go:875).

**Approach:** For each site, locate every call site (`gopls references`), confirm the value is genuinely unused, then either drop the parameter or rename to `_`.

**Risks/dependencies:** Both functions may be exposed via interfaces or callbacks; check before dropping.

**Effort:** XS once verified.

#### D13: gateway audit historical file browsing

**Goal:** Allow the admin UI to browse audit rows from older `*.jsonl` files in the same gateway directory, not just the most-recent one.

**Approach:**
- Add `GET /api/gateway/audit/files?gateway=NAME` returning `[{name, size, mtime}]`.
- Extend `GET /api/gateway/audit?gateway=NAME&file=<name>&limit=N` to honour `file`.
- UI: file picker dropdown next to the gateway selector; default to "newest" (current behavior).

**Effort:** S.

#### D14: gateway audit spillover to disk

**Goal:** Cap audit memory usage when many concurrent passthrough requests run with large bodies.

**Approach:**
- Replace the per-request `bytes.Buffer` in `streamPassthroughResponse` with a writer that switches to a temp file once it exceeds a threshold (e.g., 4 MB).
- On record finalization, read the temp file back into the audit row and delete it.
- Bound concurrent in-flight audit buffers to fail fast if temp-file disk fills.

**Risks/dependencies:** Temp file lifetime — must clean up on context cancellation and on Recorder.Close. Filesystem permissions match the audit dir.

**Effort:** M.

#### D16: flaky TCP test on macOS

**Goal:** Stop intermittent CI failures of `TestAcceptAndForward_DialerErrorDropsConnection` on `macos-latest`.

**Symptom:** `Dial failed after retries: dial tcp 127.0.0.1:NNNNN: connect: connection reset by peer`. The accepted side of the listener calls `SetLinger(0)` so that `Close()` immediately RSTs once the dialer fails. On macOS the RST sometimes races the SYN-ACK of the test's own dial, so the dial itself fails. The current 3-iteration `for range 3` retry loop is not enough on a slow CI runner.

**Approach:**
- Replace the bounded retry with a deadline-bounded poll: keep dialing until `time.Now().After(deadline)`, with a small backoff between attempts. ~250 ms total budget is plenty.
- Optionally drop `SetLinger(0)` for this specific test path and verify the assertion still holds — the goal is "client sees an error after dialer failure", which a graceful FIN also satisfies.

**Risks/dependencies:** None — test-only change.

**Effort:** XS.

#### D15: gateway passthrough e2e coverage

**Goal:** A scenario test in `e2e/scenarios/` that drives an a2a passthrough gateway against the stub LLM and asserts on audit log content.

**Approach:**
- Reuse `e2e/stub` to serve canned Anthropic SSE responses.
- Add `s5_gateway_passthrough_test.go` with build tag `e2e`. Start a mesh node with `client_api: anthropic`, `upstream_api: anthropic`, and the audit log enabled in a temp dir.
- Drive a request via `docker exec curl`, then read the audit JSONL via `docker exec cat` and assert on the reassembled `stream_summary.content`.
- Cover both gzip-encoded and identity upstream responses to exercise `decodeForAudit`.

**Effort:** M — adding a scenario is mechanical now that the stub and harness exist.

---

## Tier 4 — Features

(All items completed.)

---


## Tier 5 — Bug Hunting

Autonomous deep audit. The first three items (B7–B9) already have failing tests committed on `main` in TDD style; fixes land as separate commits per bug. H1–H14 are hunt tasks — directed audits of the codebase that a Claude session can work through sequentially, writing a failing test for every finding and fixing it in a follow-up commit.

**Entry point.** The user invokes this tier by task ID in a clean context. The Claude reads this entire section, starts at [Autonomous Run Protocol](#tier-5-autonomous-run-protocol), then works through [Known Bugs](#tier-5-known-bugs) first and [Hunt Tasks](#tier-5-hunt-tasks) second. Progress is recorded by moving items to DONE.md as each is completed. The session stops only for the exit conditions in the protocol.

### Tier 5 Autonomous Run Protocol

Read this before starting any work in Tier 5.

**Commit cadence (TDD):**

1. For every bug, the first commit introduces a failing test that reproduces the exact failure. Use the existing tests for B7–B9 as the template — one `Test<Bug>_<Scenario>` function or subtest per failure mode, with a comment naming the bug ID and the one-line reason the test fails today.
2. The second commit is the minimal fix that makes the test pass. No drive-by cleanup, no adjacent refactors.
3. An optional third commit refactors for clarity only if the minimal fix left the code ugly. Tests must still pass after the refactor.
4. Never squash (a) and (b) into one commit — the point is that `git bisect` can confirm (a) reproduces and (b) fixes.
5. Every commit runs `task check` (or `FAST=1 task check` while iterating). Do not present commits that fail the gate.
6. When a hunt turns up zero findings after the methodology has been applied in full, land a short `docs: <Hn> audit — no findings` commit with a one-paragraph note in the hunt's detail section below. Don't skip silently.

**Move-to-DONE cadence:**

- The moment a bug is fixed and committed, move its row from Tier 5 to DONE.md (per the implementation guideline). The hunt ID stays in Tier 5 until every follow-up bug from that hunt is fixed AND the "no more findings" commit is landed.
- DONE.md entries for B7–B9 should reference the commit hash of the failing-test commit and the fix commit.

**Stop conditions.** Pause and ask the user only when:

1. A fix would change a public-facing config field or YAML schema (needs human review).
2. A bug requires >30 minutes of test runtime to reproduce (batch it for the nightly churn lane).
3. A hunt turns up a genuine design question — not "which of three equivalent refactors" but "the current contract is ambiguous between A and B, and the choice changes user-visible behavior".
4. An autonomous action would be irreversible (force push, dependency upgrade, deleting files the human may still want).

For anything else — code reads, additional test cases, refactors that preserve behavior, straightforward fixes — continue working without asking.

**Out of scope for Tier 5:**

- Performance optimization (separate tier).
- API redesigns or feature additions.
- Changes to production-deployment tooling.
- Anything that requires coordinating with an external service.

**Tooling.** Each hunt nominates its primary lens (Grep, fuzz, race detector, manual read). The autonomous Claude should use the suggested lens first, then widen only if the lens misses the finding the hunt is targeting.

### Tier 5 Known Bugs

All B7–B9 fixed. See DONE.md.

### Tier 5 Hunt Tasks

All hunt tasks (H1–H34) completed. See DONE.md.

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

---

## Done

See [DONE.md](DONE.md).

---

---

## Other Notes

- Auto load .env file from the current directory to load environment variables securely.
