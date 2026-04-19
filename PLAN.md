# PLAN.md

Roadmap for mesh. Last updated 2026-04-19.
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

**Done items:** When a roadmap item is finished, remove it from PLAN.md (the table row and any detail section). Git history is the audit trail.

---

## Filesync

Filesync roadmap (performance, correctness, conflict handling, differentiation
features) lives in [`docs/filesync/PLAN.md`](docs/filesync/PLAN.md). All
filesync items are tracked there.

---

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| [P21](#p21-hot-path-discovery-sweep) | all | Hot-path discovery sweep | Profile-driven audit of where `mesh up` spends CPU and allocates memory outside `internal/filesync`. Ships a short-list of concrete optimization items into this PLAN. |
| P14  | tunnel | Parallel target probing (Happy Eyeballs) | Stagger-start all targets concurrently in `probeTarget` instead of sequential. First to connect wins, respecting target order preference within a short tie-break window. Reduces worst-case probe time from sum(timeouts) to max(stagger, fastest_target). Nice-to-have. |

#### P21: Hot-path discovery sweep

**Goal.** Identify the next set of meaningful optimization targets across the codebase after the scan-time ignore matcher was retired from the hotspot list (PF, 17× on the realistic corpus). The output of this sweep is *not* a fix — it is a prioritized list of new `Pxx` entries added to this table with ns/op and B/op baselines attached.

**Scope (inclusion).**
- SSH tunnel data path: `internal/tunnel` — `BiCopy`, keepalive loops, per-connection goroutine lifecycle, auth method build on reconnect.
- SOCKS5 / HTTP CONNECT proxy: `internal/proxy` — per-request goroutine count, address parsing, header handling.
- Clipsync: `internal/clipsync` — `SyncPayload` gzip+proto round-trip, UDP beacon fan-out, clipboard I/O polling.
- Gateway: `internal/gateway` — Anthropic↔OpenAI translation (tool-call and SSE hot paths), audit recorder, body buffering.
- Admin server: `cmd/mesh` — `/api/state` JSON encoding (every dashboard refresh), `/api/metrics` Prometheus render, SPA asset serving.
- Log ring: `cmd/mesh/humanLogHandler` — per-record classification, fan-out to file + ring.
- `state.Global` — `Snapshot()` deep copy frequency, mutex contention under many listeners / forwards, `StartEviction` walk cost.
- Filesync already covered under `docs/filesync/PLAN.md`; this sweep explicitly excludes it.

**Method.**
1. Build `mesh` with `-cpuprofile` / `-memprofile` hooks wired to the admin API (a `/debug/pprof` tab on the admin server already exists — confirm it covers the goroutines, heap, CPU, mutex, and block profiles and pin the smallest viable set).
2. Run the binary on a representative node for ≥10 minutes under realistic load (clipsync enabled, gateway idle-with-audit-metadata, one or two SSH tunnels, filesync with the production folder set). Capture one CPU profile, one heap profile, and one mutex profile.
3. For each top-10 cumulative function in each profile: determine whether (a) it is a genuine hotspot, (b) it is instrumentation overhead, or (c) it is already tracked under an existing plan item. Document the decision one line per function.
4. For every genuine hotspot not already tracked: file a `Pxx` entry in the performance table with a baseline measurement (`go test -bench` where possible, or a profile excerpt when a bench does not exist). No fix lands as part of P21 — it is discovery only.
5. Benchmark harnesses that do not exist yet (e.g. admin JSON encoding, gateway translation round-trip) are added as part of the sweep so the new items are grounded in reproducible numbers.

**Expected categories of finding.**
- **JSON / proto marshal costs on admin and state endpoints.** `/api/state` runs on every SPA poll; large state snapshots may allocate on every hit.
- **Address parsing and sorting in the dashboard.** `addrKey` / `compareAddr` are already optimized; verify.
- **Gateway SSE translation.** Per-event translation path is a candidate for allocation pressure on long streams.
- **Clipsync UDP fan-out.** Group filter and broadcast scheduling on a busy LAN.
- **Proxy per-request overhead.** Goroutine spin-up cost is rarely measured.

**Out of scope for P21.**
- Implementing any of the fixes it discovers. Every fix lands under its own `Pxx` entry with its own commit.
- Filesync work — tracked entirely in `docs/filesync/PLAN.md`.
- Micro-optimizations with no visible profile signal. Quantify before touching.

**Effort.** S–M for the sweep itself (profile capture, baseline benches, triage). The individual follow-up items are sized when filed.

**Deliverable.** A commit that adds 3–10 new `Pxx` rows to this table with baselines, plus any new benchmark files under `_test.go` used to ground the numbers.

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

### Protocol Compatibility

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| C6   | sshd      | Server keepalive uses non-standard type     | `keepalive@golang.org` — non-Go clients may not reply. |
| C7   | sshd      | Public-key auth only on server side         | No password/keyboard-interactive server auth. Asymmetry may surprise users. |

### DevOps

> **Cross-cutting test quality work** lives in [`TEST-QUALITY.md`](TEST-QUALITY.md) — rubric, per-package audit, and anti-pattern catalog. That document supersedes D3, D15, and D16 as isolated line items.

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| [D1](#d1-log-rotation) | ops | Log rotation | Unbounded growth. SIGHUP + size-based rotation or external logrotate. |
| [D2](#d2-systemd--launchd-service-units) | ops | systemd / launchd service units | No service management. Ship templates. |
| [D6](#d6-binary-signing-cosignsigstore) | release | Binary signing | No cosign/Sigstore. |
| D8   | ops       | `time.Sleep` in `downCmd` and tests         | Replace with channel-based sync. |
| [D10](#d10-darwinarm64-dist-allows-cgo) | build | darwin/arm64 dist allows CGO | Align Taskfile with GoReleaser. |
| [D11](#d11-modernize-waitgroupgo-migration) | refactor | `sync.WaitGroup.Go` migration (22 sites)    | Mechanical migration deferred — needs per-site review for panic recovery and error propagation. |
| [D12](#d12-modernize-unused-parameters) | refactor | Unused parameters in tunnel.go (2 sites)    | `unusedparams` analyzer flagged `id` and `t0`. Removing them changes function signatures; must check call sites. |
| [D13](#d13-gateway-audit-historical-file-browsing) | gateway | Audit UI shows only newest jsonl file       | `/api/gateway/audit` and the UI tab read only the most-recent file in each gateway dir. After rollover, older rows become invisible to the UI. |
| [D14](#d14-gateway-audit-spillover-to-disk) | gateway | Audit body buffer is in-memory (64 MB cap)  | Single in-flight passthrough response is bounded; multi-tenant or pathological cases should spill to a temp file. |

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

---

## Tier 5 — Bug Hunting

Autonomous deep audit. The first three items (B7–B9) already have failing tests committed on `main` in TDD style; fixes land as separate commits per bug. H1–H14 are hunt tasks — directed audits of the codebase that a Claude session can work through sequentially, writing a failing test for every finding and fixing it in a follow-up commit.

**Entry point.** The user invokes this tier by task ID in a clean context. The Claude reads this entire section, starts at [Autonomous Run Protocol](#tier-5-autonomous-run-protocol), then works through [Known Bugs](#tier-5-known-bugs) first and [Hunt Tasks](#tier-5-hunt-tasks) second. The session stops only for the exit conditions in the protocol.

### Tier 5 Autonomous Run Protocol

Read this before starting any work in Tier 5.

**Commit cadence (TDD):**

1. For every bug, the first commit introduces a failing test that reproduces the exact failure. Use the existing tests for B7–B9 as the template — one `Test<Bug>_<Scenario>` function or subtest per failure mode, with a comment naming the bug ID and the one-line reason the test fails today.
2. The second commit is the minimal fix that makes the test pass. No drive-by cleanup, no adjacent refactors.
3. An optional third commit refactors for clarity only if the minimal fix left the code ugly. Tests must still pass after the refactor.
4. Never squash (a) and (b) into one commit — the point is that `git bisect` can confirm (a) reproduces and (b) fixes.
5. Every commit runs `task check` (or `FAST=1 task check` while iterating). Do not present commits that fail the gate.
6. When a hunt turns up zero findings after the methodology has been applied in full, land a short `docs: <Hn> audit — no findings` commit with a one-paragraph note in the hunt's detail section below. Don't skip silently.

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

All B7–B9 fixed.

### Tier 5 Hunt Tasks

All hunt tasks (H1–H34) completed.

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

## Ignored

Items considered and deliberately skipped.

| ID | Item | Reason |
|----|------|--------|
| RD1 | Static IP fallbacks for `.local` targets | DNS result cache (P15, `384423c`) already does this dynamically via `resolvedAddrCache`. Hardcoding IPs is fragile (DHCP leases change) and redundant. |

---

## Other Notes

- Auto load .env file from the current directory to load environment variables securely.
