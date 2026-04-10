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
| [C3](#c3-thinking-blocks-silently-dropped-in-gateway-translation) | gateway | `thinking` blocks silently dropped | Extended thinking content dropped in both translation directions. Increasingly used feature. Needs design decision. |
| [C4](#c4-response_format-silently-dropped-in-gateway-translation) | gateway | `response_format` silently dropped | `json_object` mode parsed but dropped. Clients expecting guaranteed JSON get unstructured text. Needs design decision. |

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

## Tier 3 — Improve

Performance, UX, reliability, code quality, documentation, DevOps.

### Robustness & Error Handling

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| S11  | clipsync  | UDP beacon port used for SSRF               | `msg.GetPort()` from unauthenticated beacon. Mitigated by fixing S2. |

### Performance

| ID   | Component | Item                                         | Notes |
|------|-----------|----------------------------------------------|-------|
| [P3](#p3-adaptive-watchscan) | filesync | Adaptive watch/scan | Self-tuning heuristic. Design below. |

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
| Per-row metrics | SIMPLIFY | tx/rx only on "producer" rows (listeners, active forwards). |
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

| ID | Item | Failing test | Fix landed |
|----|------|--------------|------------|
| [B7](#b7-peermatchesaddr-ipv6-canonicalization)  | filesync peerMatchesAddr IPv6 canonicalization       | `TestPeerMatchesAddr_IPv6Canonical`            | — |
| [B8](#b8-peermatchesaddr-hostname-resolution)    | filesync peerMatchesAddr hostname DNS resolution     | `TestPeerMatchesAddr_HostnameResolution`       | — |
| [B9](#b9-loadformatsfromdir-per-format-cap)      | clipsync loadFormatsFromDir ignores MaxFileCopySize  | `TestLoadFormatsFromDir_PerFormatCapIgnoresConfig` | — |

#### B7: peerMatchesAddr IPv6 canonicalization

**Failing test:** `internal/filesync/filesync_test.go` `TestPeerMatchesAddr_IPv6Canonical`, three subtests.

**Symptom:** `peerMatchesAddr` in `internal/filesync/protocol.go` does a literal string compare on the host portion. Two IPv6 addresses that are numerically identical but written in different canonical forms (`2001:db8::1` vs. `2001:db8:0:0:0:0:0:1`, short vs. long zero runs, lowercase vs. uppercase hex) fail to match and the incoming request is rejected with 403 `unknown peer`. Silent filesync failure on any IPv6-first network.

**Fix approach:** Parse both sides with `net.ParseIP` and compare via `netip.Addr` (or `net.IP.Equal`). Fall back to string compare when either side does not parse (keeps the hostname case working). See also B8 — the fix should be designed alongside it so the function's overall shape is coherent.

**Acceptance:** All three subtests pass. The existing `TestPeerMatchesAddr` table-driven cases still pass. No new allocations in the common IPv4 path (benchmark-adjacent, so eyeball).

#### B8: peerMatchesAddr hostname resolution

**Failing test:** `internal/filesync/filesync_test.go` `TestPeerMatchesAddr_HostnameResolution`. Skips on machines where `os.Hostname()` is not resolvable to a loopback IP; fails on machines where it is (Linux CI typically).

**Symptom:** `peerMatchesAddr` never calls `net.LookupHost`, so a peer configured as `server:7756` (hostname) never matches a request from `172.20.0.3` (the resolved IP). The S2 filesync scenario works around this with a sh wrapper that resolves peer aliases at container start and rewrites a placeholder in the YAML. Users configuring docker compose service names, Tailscale magicdns, `.local` mDNS, or any LAN hostname hit a silent 403.

**Fix approach:** Two reasonable options — decide between them based on how much latency is acceptable on the hot path.

1. **Resolve at config load time.** When `FilesyncCfg.Resolve` runs, expand each configured peer's hostname to the full set of IPs via `net.LookupHost` and store the IP set. `peerMatchesAddr` becomes a pure membership check. Cost: stale entries on DNS changes; startup blocks on DNS.
2. **Resolve on demand with cache.** `peerMatchesAddr` runs DNS only when the literal compare fails, caches results for N minutes. Cost: per-request latency on first miss; cache invalidation.

Option 1 is simpler and matches mesh's "validate at load" style. Prefer it unless there is a concrete reason users need hot-reload DNS behavior — in which case pause and ask.

**Acceptance:** The test fails cleanly on a machine where `os.Hostname()` resolves to loopback (reproduce by adding a line to `/etc/hosts` locally or running on Linux CI), then passes after the fix. All existing filesync tests continue to pass.

#### B9: loadFormatsFromDir per-format cap

**Failing test:** `internal/clipsync/clipsync_test.go` `TestLoadFormatsFromDir_PerFormatCapIgnoresConfig`.

**Symptom:** `defaultMaxSyncFileSize` in `internal/clipsync/clipsync.go` has a docstring saying "Overridden by `ClipsyncCfg.MaxFileCopySize`", but `loadFormatsFromDir` at line 1034 uses the literal constant instead of `n.maxFileSize`. The function has no receiver and no cap parameter, so it physically cannot see the config. A user raising `max_file_copy_size` to 200 MB still loses any clipboard text format over 50 MB silently, with a `WARN Skipping clipboard format: exceeds per-file size limit` line in the log.

**Fix approach:** Thread the effective cap through to the assembler. Two options:

1. **Parameter:** change the signature to `loadFormatsFromDir(dir string, maxFileSize int64) []*pb.ClipFormat` and pass `n.maxFileSize` from the one caller in `readClipboardFormats`. Same for `maxClipboardPayload` if the plan is for both caps to be configurable (the latter is separate — do not bundle).
2. **Receiver:** promote `loadFormatsFromDir` to a method on `*Node`. Lighter diff than adding a parameter, and consistent with other private helpers that already reference `n.maxFileSize`.

Option 2 is preferred unless there is a test that calls `loadFormatsFromDir` without a Node — check before deciding.

**Acceptance:** `TestLoadFormatsFromDir_PerFormatCapIgnoresConfig` passes. `TestLoadFormatsFromDir`, `TestLoadFormatsFromDir_EmptyDir`, `TestLoadFormatsFromDir_SkipsEmptyFiles`, `TestLoadFormatsFromDir_IgnoresUnknownFiles` continue to pass. Add a second subtest that asserts a 60 MB format IS still rejected when `MaxFileCopySize` is left at its default of 50 MB — that pins the default behavior.

### Tier 5 Hunt Tasks

H1–H14 were the first pass surfaced directly by the D11 e2e work. H15–H30 come from a second-pass review and align with the generic bug-hunt skill (`~/.claude/skills/bug-hunt/SKILL.md`). For each hunt below, the skill entry in brackets points at the full methodology; this PLAN.md row names the mesh-specific files, functions, and attack inputs to audit.

**Input boundaries**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H1](#h1-address-and-host-equality-audit)                | Address / host / URL equality across packages            | I.1     | Grep + read |
| [H7](#h7-path-traversal-audit)                           | File paths derived from peer / user input                | I.2     | Grep + read |
| [H8](#h8-integer-overflow-audit)                         | Size arithmetic near int64 bounds                        | I.3     | Read        |
| [H10](#h10-unbounded-io-audit)                           | Network-derived reads without `io.LimitReader`           | I.4     | Grep        |
| [H15](#h15-subprocess-and-command-injection-audit)       | Subprocess / command injection (password_command, exec) | I.5     | Read        |
| [H16](#h16-redos-audit)                                  | Regex catastrophic backtracking on peer input            | I.8     | Read        |
| [H17](#h17-log-injection-audit)                          | CRLF / ANSI / control chars in logged peer fields        | I.9     | Grep + read |

**Cryptography and transport**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H18](#h18-cryptographic-correctness-audit)              | Hash choice, RNG source, constant-time compare           | III.1   | Grep + read |
| [H19](#h19-tls-posture-audit)                            | TLS / certificate verification skipped or weakened       | III.2   | Grep        |

**Concurrency**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H3](#h3-concurrent-close-audit)                         | `Close()` / `cancel()` from multiple goroutines          | IV.1    | Race + read |
| [H4](#h4-goroutine-leak-audit)                           | `go func(){...}()` with no context exit path            | IV.2    | Read + race |
| [H5](#h5-timer-and-ticker-audit)                         | `time.NewTimer` / `NewTicker` without `Stop`            | IV.3    | Grep        |
| [H6](#h6-signal-handler-audit)                           | `signal.Notify` without matching `signal.Stop`          | IV.4    | Grep        |
| [H20](#h20-context-propagation-audit)                    | `ctx` accepted but not forwarded to inner IO calls       | IV.5    | Grep + read |
| [H21](#h21-channel-send-close-correctness-audit)         | Send to closed channel, double close, nil channel       | IV.6    | Read + race |
| [H22](#h22-atomic-read-modify-write-audit)               | Shared counters / flags updated non-atomically           | IV.7    | Read + race |
| [H23](#h23-lock-ordering-audit)                          | Two locks acquired in different orders on different paths | IV.8  | Read        |
| [H11](#h11-map-iteration-safety-audit)                   | Iteration over shared maps without mutex                 | IV.9    | Race + read |

**Resource management**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H24](#h24-http-client-hygiene-audit)                    | HTTP clients missing timeouts / body close / pool caps   | V.3     | Grep + read |
| [H12](#h12-http-server-hygiene-audit)                    | `http.Server` missing timeouts / size caps               | V.4     | Grep        |

**Error handling**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H9](#h9-error-handling-audit)                           | Silent error swallowing (`_ = err`, empty catch)         | VI.1    | Grep + read |
| [H25](#h25-error-wrapping-audit)                         | `fmt.Errorf` without `%w`; broken `errors.Is`/`As` chains | VI.2   | Grep + read |
| [H26](#h26-retry-backoff-audit)                          | Retry loops without bounded backoff or attempt budget    | VI.3    | Read        |

**Type and logic correctness**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H2](#h2-default-constant-vs-runtime-config-audit)       | "default" constants shadowing runtime config             | VII.4   | Grep + read |
| [H27](#h27-typed-nil-audit)                              | Nil pointer wrapped in non-nil interface                 | VII.1   | Read        |
| [H28](#h28-enum-switch-exhaustiveness-audit)             | Switches over `state.Status` and enums without default   | VII.3   | Grep + read |

**Filesystem**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H29](#h29-toctou-and-atomic-write-audit)                | Stat-then-open races; non-atomic persistent writes       | VIII.1 + VIII.2 | Read |
| [H30](#h30-cross-platform-path-audit)                    | Hardcoded `/` separators, case sensitivity, long paths   | VIII.5  | Grep + read |

**Network parsers**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H13](#h13-sse-parser-edge-cases)                        | Gateway SSE parser with split / malformed frames         | IX.1    | Fuzz        |

**Data and serialization**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H14](#h14-config-parser-adversarial-inputs)             | YAML loader with adversarial inputs                      | X.1     | Fuzz        |

**State and lifecycle**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H31](#h31-shutdown-drain-audit)                         | In-flight requests dropped on SIGTERM                    | XI.3    | Read        |
| [H32](#h32-state-machine-invariant-audit)                | Unreachable transitions; missing terminal states         | XI.4    | Read        |

**Environment and platform**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H33](#h33-clock-monotonic-audit)                        | Wall-clock used where monotonic is required              | XII.1   | Grep        |
| [H34](#h34-environment-variable-audit)                   | Missing env vars silently becoming empty defaults        | XII.2   | Grep        |

#### H1: Address and host equality audit

**Goal.** B7 and B8 are both in `internal/filesync/protocol.go`. The same bug class likely exists in `internal/tunnel` (SSH target matching, `PermitOpen` check), `internal/clipsync` (trusted peers, beacon source filtering), and `internal/proxy` (ACLs if any). Find every function that compares IP or host strings and ensure it parses through `net.ParseIP` / `netip.Addr` and optionally resolves hostnames.

**Methodology.**

1. `Grep -nI '== *req\|== *host\|== *addr\|== *remote\|strings.EqualFold.*[Aa]ddr' internal/`.
2. For each hit, read the function. Confirm whether it is comparing a user-configured value to a runtime peer address. If so, construct a test case with two equivalent-but-different forms (see B7 for IPv6; add an IPv4 bracketed form like `::ffff:127.0.0.1`; add a mixed-case hostname).
3. Write a failing test per finding in the neighbouring `_test.go` file, using the B7 `t.Run` pattern.
4. Fix by replacing string compare with `ParseIP`+`.Equal` (for IP) or a shared helper like `addrEqual(a, b string) bool` (for host-or-IP).
5. Landing order inside the hunt: filesync first (already partially covered by B7/B8), then tunnel, then clipsync, then proxy.

**Test pattern.** Table-driven, same shape as `TestPeerMatchesAddr_IPv6Canonical`. Each table row names a canonicalization the function should normalize through.

**Fix pattern.** Add a package-local helper `hostEqual(a, b string) bool` that ParseIPs both sides; fall through to a `net.LookupHost`-backed fallback if either side is a hostname. Reuse across packages via an `internal/netutil` export if the shape is identical.

**Acceptance.** Zero remaining string-compare-of-addresses hits in the Grep. All new tests passing. `FAST=1 task check` green.

#### H2: Default constant vs runtime config audit

**Goal.** B9 showed that `defaultMaxSyncFileSize` is wired into the assembler as a literal constant despite a docstring saying it is overridable. Find every other `const default*` in `internal/` and `cmd/` where the docstring promises configurability and the code hardcodes the constant.

**Methodology.**

1. `Grep -nI 'default[A-Z][A-Za-z]* *=' internal/ cmd/`.
2. For each constant, read its docstring. If it says "Overridden by" / "Default for" / "Configurable via", trace every usage with `Grep`.
3. For each usage that references the constant by name instead of the equivalent runtime field, check whether the caller has access to config. If yes, it is a bug.
4. Write a failing test that sets the runtime field to a value different from the default and asserts the different value is honored.

**Test pattern.** Configure the runtime field to a non-default value, exercise the path, assert the runtime value took effect. If the runtime value is used transparently, the test passes.

**Fix pattern.** Either thread the runtime field to the usage site (parameter or receiver), or document the constant as a non-overridable hard cap and fix the docstring. Prefer threading unless the docstring "override" was aspirational rather than a contract.

**Acceptance.** Every `const default*` in the repo either (a) has no "overridable" claim in its docstring, or (b) is threaded through to every one of its usage sites. New tests pin the configurability for each one.

#### H3: Concurrent Close audit

**Goal.** CLAUDE.md already has a "don't call `Close()` from multiple goroutines without `sync.Once`" rule, and B4 (agent fwd double close) was a prior incident. Audit every `Close()` / `cancel()` call that may be reachable from more than one goroutine.

**Methodology.**

1. `Grep -nI '\.Close\(\)\|cancel *()' internal/ cmd/`.
2. For each hit, identify the owning goroutine. If the target resource is shared with another goroutine (channel, listener, connection, process), confirm there is a `sync.Once`-guarded wrapper.
3. Pay special attention to patterns like `defer <thing>.Close(); go func() { ... <thing>.Close() ... }()` — the classic double-close.
4. Reproduce any finding with a `-race` test that starts two goroutines racing on the close.

**Test pattern.** Use `sync.WaitGroup` and two goroutines that both reach the close path concurrently, run with `-race` to catch the double-close panic or the racy write.

**Fix pattern.** Wrap the close in a local closure guarded by `sync.Once` (see `onceCloseListener` in `tunnel.go` for the idiom).

**Acceptance.** Every closeable shared resource has a single-entry close path or an explicit `sync.Once`. Any finding lands as test+fix commits.

#### H4: Goroutine leak audit

**Goal.** Long-running daemons leak goroutines when one of the "go func(){}" statements has no context-tied termination path. Find any goroutine that does not exit cleanly on process shutdown.

**Methodology.**

1. `Grep -nI 'go func\b\|go [a-zA-Z_]*(' internal/ cmd/`.
2. For each launch site, identify the goroutine's exit condition. Acceptable exits: `<-ctx.Done()` in a select, `for msg := range ch` where `ch` is closed by a controlled writer, `return` after finishing a bounded task.
3. Unacceptable: infinite `for {}` with no exit, `time.Sleep` without context, blocking on a channel nothing closes.
4. For each leak, write a test using `runtime.NumGoroutine()` to assert that `Start(ctx)` followed by `cancel()` + a short wait returns to baseline.

**Test pattern.**

```go
base := runtime.NumGoroutine()
ctx, cancel := context.WithCancel(context.Background())
// start component
cancel()
// brief settle
for i := 0; i < 10 && runtime.NumGoroutine() > base; i++ { time.Sleep(50 * time.Millisecond) }
if runtime.NumGoroutine() > base+1 { t.Fatalf(...) }
```

**Fix pattern.** Add `select { case <-ctx.Done(): return; case x := <-ch: ... }` or wire `context.AfterFunc` to trigger a clean exit.

**Acceptance.** Every `go func` in the main runtime has a documented exit path. Tests pin the cleanup for each Start/Stop pair.

#### H5: Timer and ticker audit

**Goal.** CLAUDE.md says "Don't use `time.After` in select with `ctx.Done()`" and "use `time.NewTimer` with explicit `Stop()`". Audit.

**Methodology.**

1. `Grep -nI 'time\.After\|time\.NewTimer\|time\.NewTicker' internal/ cmd/`.
2. For `time.After`: any hit inside a select that also has `ctx.Done()` is a leak on cancellation (the timer's goroutine sticks around until the deadline). Replace with `time.NewTimer` + `Stop` in the ctx branch.
3. For `time.NewTimer` / `NewTicker`: every creation must have a matching `Stop` on every exit path.

**Test pattern.** `runtime.NumGoroutine()` delta across a Start/cancel cycle, same as H4.

**Fix pattern.** Standard `t := time.NewTimer(d); defer t.Stop()` + select on `t.C` and `ctx.Done`.

**Acceptance.** Zero `time.After` hits in hot paths. Every `NewTimer` / `NewTicker` has an explicit Stop.

#### H6: Signal handler audit

**Goal.** CLAUDE.md: "Don't call `signal.Notify` without a matching `defer signal.Stop`". Audit.

**Methodology.**

1. `Grep -nI 'signal\.Notify' cmd/ internal/`.
2. For each hit, confirm there is a `signal.Stop(ch)` on every exit path (including panic recovery). A missing `Stop` leaks the signal handler across the process's lifetime — benign in `main` but a real leak anywhere else.
3. Write a test that calls the helper twice in a row and asserts no duplicate delivery.

**Test pattern.** Call setup + teardown twice; send a signal; assert the handler count matches expectation.

**Fix pattern.** `defer signal.Stop(ch)` adjacent to the `Notify` call.

**Acceptance.** Every `signal.Notify` is paired.

#### H7: Path traversal audit

**Goal.** Filesync accepts file names from remote peers. SFTP serves files from a configured root. Clipsync file_copy accepts file names. All three are attack surfaces for `../../etc/passwd`-style escapes. Audit.

**Methodology.**

1. `Grep -nI 'filepath\.Join\|os\.Open\|os\.Create\|os\.WriteFile\|os\.MkdirAll' internal/filesync/ internal/clipsync/ internal/tunnel/`.
2. For each hit, identify whether any component of the path came from peer input (request body, protobuf field, URL).
3. If so, confirm the code calls `filepath.Clean` **and** checks that the result is still inside the configured root (prefix check against an absolute-cleaned root).
4. Attack vectors to exercise in tests: `../escape`, `..\escape` (Windows), absolute paths (`/etc/passwd`), symlinks (create one in the sync dir pointing outside), long path names, names with NUL bytes.

**Test pattern.** Send a peer payload with a malicious filename; assert the write is rejected or clamped inside the root.

**Fix pattern.** Common helper: `func safeJoin(root, name string) (string, error)` that cleans, rejects absolute, rejects `..` segments, rejects anything whose cleaned absolute path does not begin with the cleaned absolute root.

**Acceptance.** No peer-derived path flows into `os.*` or `filepath.Join` without passing through `safeJoin` or an equivalent.

#### H8: Integer overflow audit

**Goal.** File sizes, offsets, delta block counts, and buffer lengths are all `int64` or `int`. A malicious peer or a 10 PB folder could overflow if the arithmetic is careless.

**Methodology.**

1. `Grep -nI 'int64\|\.Size()\|len\(' internal/filesync/ internal/clipsync/ internal/gateway/`.
2. Focus on: `a + b` where both are large, `a * n` where `n` is user-supplied, slice allocations with `make([]byte, n)` where `n` is peer-derived.
3. Write fuzz tests that feed the parsers with values near `math.MaxInt64`.
4. For each overflow, guard with `if a > math.MaxInt64 - b { return error }` or use `math/big` on the ingest side.

**Test pattern.** Table-driven arithmetic test with boundary inputs.

**Fix pattern.** Explicit overflow check before the arithmetic, or use a saturating helper.

**Acceptance.** Every `int64` arithmetic op on peer input either checks bounds or is provably safe (e.g., bounded by a prior size cap).

#### H9: Error handling audit

**Goal.** `_ = err` and empty catch blocks hide real failures. Find every swallowed error in non-cleanup paths.

**Methodology.**

1. `Grep -nI '_ = err\|_ = .*\.Close\(\)' internal/ cmd/`.
2. `Grep -nI 'if err != nil {\s*}' internal/ cmd/` (ripgrep multiline).
3. For each hit, judge whether the error is genuinely safe to ignore (cleanup path, already-reported error, best-effort) or whether a real failure could be lost.
4. For borderline cases, log at `slog.Warn` with context rather than swallowing silently.

**Test pattern.** Inject an error from a mock and assert it is either returned, logged, or has a documented reason for being ignored.

**Fix pattern.** Wrap with `slog.Warn("context", "err", err)` or return.

**Acceptance.** Every swallowed error has either a test asserting it is OK, or a one-line comment explaining why (a trailing `// cleanup: best-effort` style).

#### H10: Unbounded io audit

**Goal.** Any `io.ReadAll` / `io.Copy` / `json.NewDecoder(r).Decode` on network-derived input without an `io.LimitReader` is a DoS.

**Methodology.**

1. `Grep -nI 'io\.ReadAll\|io\.Copy\|json\.NewDecoder\|proto\.Unmarshal' internal/`.
2. For each, trace the reader backwards. If it comes from `http.Request.Body`, `net.Conn`, or a protobuf field whose size is not pre-validated, the call is unbounded.
3. Wrap with `io.LimitReader(r, cap)` or use `http.MaxBytesReader`. The caps should match existing package conventions (`maxRequestBodySize`, `maxUpstreamResponseSize`, etc.).

**Test pattern.** Feed a 1 GB reader and assert the handler returns a 413 / appropriate error without OOMing.

**Fix pattern.** `body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))`.

**Acceptance.** Zero unbounded reads on peer-derived streams.

#### H11: Map iteration safety audit

**Goal.** Iterating a shared map without the owning mutex is a race. Audit.

**Methodology.**

1. `Grep -nI 'for .* := range [a-zA-Z_][a-zA-Z0-9_]*$\|for .* := range .*\.[a-zA-Z_][a-zA-Z0-9_]*$' internal/`.
2. For each, check whether the map is concurrently written elsewhere. `state.Global.Snapshot` is the correct pattern — copy under the lock, then iterate the copy.
3. Build race tests by spawning a writer goroutine and an iterator goroutine, then run with `go test -race`.

**Test pattern.** Start a writer + an iterator in goroutines; assert the `-race` detector stays quiet.

**Fix pattern.** Snapshot under mutex, iterate the snapshot; or hold the read lock for the entire iteration.

**Acceptance.** `go test -race -count=1 ./...` stays green after the hunt, and any finding is pinned by a new race test.

#### H12: HTTP server hygiene audit

**Goal.** Every `http.Server` in mesh should have `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and `MaxHeaderBytes`. Missing any of these is a slowloris / slow-header vector.

**Methodology.**

1. `Grep -nI '&http.Server{\|http\.Server{' internal/ cmd/`.
2. For each, list which timeouts are set. Anything with only `ReadHeaderTimeout` is incomplete.
3. The gateway package already has `ReadHeaderTimeout` on its client; audit whether the server side is as careful.

**Test pattern.** Use a slow client that dribbles bytes over a long window; assert the server disconnects within the expected window.

**Fix pattern.** Set all five fields explicitly with project-wide constants (introduce `netutil.DefaultHTTPTimeouts()` if duplication becomes annoying).

**Acceptance.** Every `http.Server` has all five fields set.

#### H13: SSE parser edge cases

**Goal.** The gateway reads SSE from upstream and re-emits it. Parsers that assume frames arrive aligned on `\n\n` boundaries are fragile; malformed upstreams (missing terminator, embedded `\n` in data, giant fields) can crash or deadlock the parser.

**Methodology.**

1. Read the SSE parse path in `internal/gateway/a2o_stream.go` and `o2a_stream.go`.
2. Write a fuzz target that feeds arbitrary byte sequences to the parser and asserts no panic and no unbounded memory growth.
3. Write unit tests for specific edge cases: frame spans multiple reads, empty `data:` line, multi-line `data:` fields, `event:` without `data:`, huge single field.

**Test pattern.** `FuzzSSEParser` target using `testing.F`; seeded with the stub-llm's canned streams.

**Fix pattern.** Use a size cap on the internal buffer (already documented: 4 MB for SSE lines). Verify it is enforced on every read.

**Acceptance.** Fuzz runs 10^6 inputs without panic or OOM. New unit tests pin each edge case.

#### H14: Config parser adversarial inputs

**Goal.** `internal/config` already has `fuzz_test.go`. Extend it with adversarial inputs that target the attack surface: YAML anchor bombs, deep nesting, duplicate keys, huge strings, unknown environment variables, `os.ExpandEnv` with `${...}` expressions that leak host env.

**Methodology.**

1. Read the existing fuzz target. Add seeds for: anchor-bomb YAML (alias chains), duplicate-key YAML (both accepted and rejected paths), `admin_addr` with IPv6 zone IDs, `env` vars set to `${PWD}`, `peers:` with self-reference.
2. Run `go test -fuzz=FuzzConfigLoad -fuzztime 2m ./internal/config/...` periodically during the hunt.
3. Every crash or non-clean failure becomes a failing test seeded from the fuzz corpus.

**Test pattern.** Promote the fuzz crash input to a regression test; the fuzz target stays.

**Fix pattern.** Validate shape at load time; reject malformed inputs with a clear error.

**Acceptance.** 2 minutes of `go test -fuzz=FuzzConfigLoad` produces zero new crashes. Every prior crash is pinned by a regression test.

#### H15: Subprocess and command injection audit

**Goal.** Mesh shells out in three places: `password_command` (user-configured shell string executed to fetch an SSH password), the SSH server session shell (`shell: [bash, -l]` per config), and anywhere a helper binary is invoked. Each one is a potential injection surface if any part of the command comes from peer input.

**Mesh sites.** `internal/tunnel/` password_command execution, `internal/tunnel/shell_unix.go` and `shell_windows.go` for session shell spawn, any `exec.Command` / `exec.CommandContext` call across `internal/`.

**Methodology.** Skill §I.5 methodology. Grep for `exec.Command`, `shellCommand`, `sh -c`, ``, `/C ` (Windows cmd), `pwsh -Command`. For each hit, trace every argument back to its source and confirm none come from peer input without being passed as a separate argv element.

**Attack inputs.** Usernames with `;`, `&&`, `$()`, backticks, newlines, backslashes; passwords containing shell metacharacters; SSH session `exec` payloads containing shell redirects.

**Fix pattern.** Argv-form exec; never shell out with user-controlled string interpolation. Where a shell is unavoidable (password_command is designed to run shell), document the trust boundary: the config file is trusted input, peer-derived usernames are not.

**Acceptance.** Zero reachable concatenation of peer input into shell command strings.

#### H16: ReDoS audit

**Goal.** Go's standard `regexp` package is RE2-based and linear-time, so classic catastrophic backtracking is not a concern. The attack shifts to *input length* — a regex that is linear but applied to a 100 MB peer-supplied string is still a DoS.

**Mesh sites.** `PermitOpen` parser in `internal/tunnel/tunnel.go`, gitignore-style pattern matching in `internal/filesync/`, the config loader's envvar substitution (`os.ExpandEnv`), any regex applied to headers or request paths in `internal/gateway/`.

**Methodology.** Skill §I.8 methodology, with the caveat that for Go the audit is about length caps, not pattern rewriting. Grep for `regexp.MustCompile`, `regexp.Compile`, `regexp.Match*`. For each hit, trace the input and confirm it is bounded before the match.

**Test pattern.** Feed a 100 MB input to each regex site and assert it completes within a 1s budget.

**Fix pattern.** `io.LimitReader` at the ingest boundary; reject input longer than a hunt-specific cap with a clear error.

**Acceptance.** Every regex site has either a proven length bound on its input or an explicit cap at the ingest boundary.

#### H17: Log injection audit

**Goal.** Find fields containing CRLF, ANSI escape sequences, or control characters that flow into log lines or (worse) HTTP response headers.

**Mesh sites.** Everywhere `slog.Info` / `slog.Warn` / `slog.Error` is called with peer-derived values: usernames from SSH auth, filenames from filesync, clipboard mime types, gateway upstream error bodies, peer addresses from `addrFromRequest`.

**Methodology.** Grep for `slog.` / `log.` calls whose arguments include any variable derived from `r.URL`, `r.Header`, `r.Body`, `conn.RemoteAddr`, `user`, `filename`. For each, confirm the logger strips or escapes control characters.

**Test pattern.** Emit a log line with CRLF in a field; assert the output has a single line (control chars stripped or escaped).

**Fix pattern.** Strip control chars in a shared helper before logging; use structured logging exclusively (slog with attrs, not format strings). Review `cmd/mesh/format.go` and the `humanLogHandler` for whether user fields get sanitized.

**Acceptance.** No peer-derived field can inject a second log line.

#### H18: Cryptographic correctness audit

**Goal.** Audit every use of a cryptographic primitive for algorithm choice, RNG source, and constant-time compare.

**Mesh sites.** `internal/filesync/` for file hashes (must be SHA-256, never MD5), `internal/clipsync/` for content hashes, SSH key generation and marshalling in `internal/tunnel/`, any signature check.

**Methodology.** Skill §III.1 methodology. Grep for `crypto/md5`, `crypto/sha1`, `math/rand`, `bytes.Equal` (should be `subtle.ConstantTimeCompare` for secrets), `hmac.Equal` vs raw compare, `crypto/rand` (good) vs `math/rand` (bad for crypto).

**Acceptance.** No MD5 or SHA-1 in security paths. No `math/rand` for any secret or token generation. All secret comparisons use `subtle.ConstantTimeCompare`.

#### H19: TLS posture audit

**Goal.** Find any TLS client that skips verification or uses a weakened config.

**Mesh sites.** HTTP clients in `internal/clipsync/` (peer pull-back), `internal/filesync/` (index exchange, file GET, delta POST), `internal/gateway/` (upstream LLM API calls). Note: filesync and clipsync HTTP are Tier 2 items S1/FS4 (planned to move to HTTPS with auto-TLS).

**Methodology.** Skill §III.2 methodology. Grep for `InsecureSkipVerify`, `tls.Config`, `http.Transport{TLSClientConfig:`. Confirm every client verifies by default.

**Acceptance.** Every outbound TLS client verifies by default; any `InsecureSkipVerify: true` is either removed or has a test and a comment explaining why.

#### H20: Context propagation audit

**Goal.** Find places where a function accepts a `context.Context` but fails to forward it to an inner IO call.

**Mesh sites.** Every handler in the admin server, clipsync server, filesync server, gateway server; every outbound HTTP call; every `exec.Command`.

**Methodology.** Skill §IV.5 methodology. Grep for `http.Get`, `http.Post`, `http.NewRequest(`, `exec.Command(`, `net.Dial(`. Each should be the `*WithContext` variant in a function that has a context available.

**Test pattern.** Cancel the parent context mid-operation; assert the inner call actually aborts, not just the wrapper loop.

**Acceptance.** Every IO call in a context-aware function uses the context variant.

#### H21: Channel send-close correctness audit

**Goal.** Find channel patterns that can panic (send to closed, close twice, close nil) or block forever.

**Mesh sites.** `internal/tunnel/` has heavy channel use for SSH session request routing; `internal/netutil/BiCopy`; state eviction goroutines; any `make(chan ...)`.

**Methodology.** Skill §IV.6 methodology. Grep for `close(` and `make(chan`. For each close, confirm there is a single owner. For each send, confirm no path closes the channel while another path may still send.

**Test pattern.** Race harness: two producers sending concurrently, one path closing. `go test -race`.

**Acceptance.** No panic from channel misuse under the race detector.

#### H22: Atomic read-modify-write audit

**Goal.** Find shared counters, flags, or caches updated without atomics or a mutex.

**Mesh sites.** `state.Global` uses a mutex correctly; audit whether there are other shared data structures that don't. `internal/tunnel/authFailuresByIP` (map of counters), component metrics (`BytesTx`/`BytesRx`/`Streams` — these look like `atomic.Int64`; verify).

**Methodology.** Skill §IV.7 methodology. Grep for `sync.Mutex`, `sync.RWMutex`, `atomic.`. For every shared variable, confirm all writers use the same synchronization primitive and that reads are consistent with writes.

**Test pattern.** Concurrent writer test; `-race`.

**Acceptance.** `-race` clean on every shared structure.

#### H23: Lock ordering audit

**Goal.** Find code paths that acquire multiple locks in different orders.

**Mesh sites.** `internal/state/state.go` (single mutex); `internal/filesync/` (several mutexes: folder, index, peers); `internal/tunnel/` (SSH client maps).

**Methodology.** Skill §IV.8 methodology. For every function that takes more than one lock, document the order. For every pair of locks, confirm no two functions take them in opposite orders.

**Test pattern.** Deadlock test: two goroutines each taking the two locks in reverse order; assert the `-race` detector or a deadlock watchdog catches it.

**Acceptance.** Lock ordering documented in package header comments; no detected cycles.

#### H24: HTTP client hygiene audit

**Goal.** Companion to H12 (server hygiene). Every outbound HTTP client needs explicit timeouts, body close on every path, and a bounded connection pool.

**Mesh sites.** `internal/clipsync/` push client, `internal/filesync/` sync client, `internal/gateway/` upstream client.

**Methodology.** Skill §V.3 methodology. Grep for `&http.Client{` and `http.Client{}`. Every instance must set `Timeout` and a custom `Transport` with bounded `MaxIdleConnsPerHost`. Every response must `defer resp.Body.Close()` on every path including errors.

**Test pattern.** Point the client at a server that hangs; assert the client gives up within the configured total timeout.

**Acceptance.** Every outbound HTTP client has bounded timeouts and a closed body on every path.

#### H25: Error wrapping audit

**Goal.** Find `fmt.Errorf` calls without `%w` that break `errors.Is` / `errors.As` chains.

**Mesh sites.** Every package that wraps errors. Mesh uses `fmt.Errorf` heavily; the convention in CLAUDE.md is `fmt.Errorf("connections[%d] %q: %w", ...)`.

**Methodology.** Skill §VI.2 methodology. Grep for `fmt.Errorf("` and confirm every call that includes an `%v` error actually uses `%w`. Identify sentinel errors (`var Err... = errors.New(...)`) and confirm `errors.Is(wrapped, Err...)` still works through the wrap chain.

**Test pattern.** Wrap a sentinel through several layers; assert the outermost error still answers `Is(sentinel)`.

**Acceptance.** No `%v` for errors where `%w` is possible; every sentinel-based `errors.Is` path has a test.

#### H26: Retry backoff audit

**Goal.** Find retry loops without exponential backoff, jitter, or a budget.

**Mesh sites.** `internal/tunnel/runForwardSet` reconnect loop, `internal/clipsync/` push retry, `internal/filesync/` sync retry, `internal/gateway/` upstream retry (if any).

**Methodology.** Skill §VI.3 methodology. Find every `for { ... err := call(); if err == nil { break } }` pattern. Confirm: exponential backoff, jitter, max-attempts cap, per-call deadline, retryable-vs-permanent error classification.

**Test pattern.** Point the client at a failing upstream; assert total time stays under budget and total call count stays under max-attempts.

**Acceptance.** Every retry loop has bounded backoff and an explicit budget.

#### H27: Typed-nil audit

**Goal.** Find Go's classic `var x *T = nil; var i Iface = x; i != nil` trap — interfaces wrapping nil concrete pointers.

**Mesh sites.** Every function that returns an interface built from a concrete pointer: error returns, state snapshot builders, listener factories.

**Methodology.** Skill §VII.1 methodology. Grep for functions returning interface types from concrete pointers (`return &MyStruct{...}` where return type is an interface). For each, verify the constructor never returns a typed nil.

**Test pattern.** `if err := newThing(); err != nil { ... }` with a constructor that can legitimately return nil; assert the interface comparison works as expected.

**Acceptance.** No typed-nil traps reachable from public constructors.

#### H28: Enum / switch exhaustiveness audit

**Goal.** Find switches over `state.Status` and other enum-like types that would silently fall through if a new value were added.

**Mesh sites.** `internal/state/state.go` defines `Status` constants (`Listening`, `Connecting`, `Connected`, `Failed`, `Retrying`, `Starting`); every switch over `Status` in `cmd/mesh/` dashboard rendering, `cmd/mesh/` format.go, admin metrics writer, filesync peer status aggregation.

**Methodology.** Skill §VII.3 methodology. Grep for `switch .*Status` / `switch .*status`. Every one should either have an explicit `default:` that logs or errors on unknown, or be paired with a comment that it is intentionally exhaustive (verified at review time).

**Test pattern.** Add a fake "unknown" status value in a test helper; switch on it; assert the default branch handles it safely.

**Acceptance.** Every status switch handles all current values and has a safe default.

#### H29: TOCTOU and atomic write audit

**Goal.** Find stat-then-open races and non-atomic persistent writes.

**Mesh sites.** `internal/config/` permission check (`checkInsecurePermissions` then `os.ReadFile`), `internal/filesync/` index persistence (`index.yaml`, `peers.yaml` — should be tmp-rename), any `os.Stat` followed by `os.Open` on the same path.

**Methodology.** Skill §VIII.1 + VIII.2 methodology. Grep for `os.Stat`, `os.Lstat` followed by `os.Open`, `os.Create`. For persistent writes, confirm the pattern is tmp-write + `fsync` + `os.Rename`.

**Test pattern.** Crash-injection test: kill the process during `os.Rename`; assert the file is either fully old or fully new, never partial.

**Acceptance.** No stat-then-open-to-act-on-stat-result patterns; all persistent writes are atomic.

#### H30: Cross-platform path audit

**Goal.** Find hardcoded `/` separators, case-sensitivity assumptions, and MAX_PATH 260 assumptions on Windows.

**Mesh sites.** `internal/filesync/` (peer-exchanged filenames must not depend on host separator), `internal/clipsync/` file copy, `internal/tunnel/sftp.go`, `cmd/mesh/` config paths.

**Methodology.** Skill §VIII.5 methodology. Grep for `"/"` in path contexts, `strings.Split(..., "/")`, `path.Join` (wrong on Windows — should be `filepath.Join`), `filepath.Separator` assumptions.

**Test pattern.** Cross-platform CI matrix covering Windows if available; explicit tests for Unicode filenames and paths with `..`.

**Acceptance.** Every path operation uses `filepath` (not `path`); no hardcoded separators.

#### H31: Shutdown drain audit

**Goal.** Find shutdown paths that drop in-flight requests or tasks.

**Mesh sites.** `cmd/mesh/main.go` SIGTERM handler; SSH server (`tunnel.NewSSHServer`) graceful shutdown; admin HTTP server; clipsync, filesync, gateway servers.

**Methodology.** Skill §XI.3 methodology. For each server, trace what happens when the parent context is cancelled. In-flight requests should complete within a bounded drain period; new requests should be rejected; the final `wg.Wait()` should not deadlock.

**Test pattern.** Start a long-running request, cancel the context, assert the request completes before the process exits.

**Acceptance.** Every server has a documented drain behavior with a test.

#### H32: State machine invariant audit

**Goal.** Find `state.Status` transitions that shouldn't be reachable but are, or terminal states that are missing.

**Mesh sites.** `internal/state/state.go` plus every `state.Global.Update` call site. Mesh does not have a formal state machine; the hunt documents the implicit one.

**Methodology.** Skill §XI.4 methodology. List every `state.Global.Update` call and the source status + target status pair. Build a transition graph. Identify transitions that bypass intermediate states (e.g., `Starting → Connected` without passing through `Connecting`) and decide whether they are bugs or intentional shortcuts.

**Test pattern.** For each documented transition, a unit test that drives the component through the expected sequence and asserts the status at each step.

**Acceptance.** A documented state machine diagram in `internal/state/` (or the package doc) plus transition tests.

#### H33: Clock and monotonic time audit

**Goal.** Find `time.Now()` used for measuring durations (wall clock can jump backwards) or for testability-critical logic.

**Mesh sites.** Retry backoff, scan intervals, eviction TTL, metric timestamps, activity timestamps, auth failure counters, cache TTL.

**Methodology.** Skill §XII.1 methodology. Grep for `time.Now()`. For each, classify as "wall clock OK" (display, logging) or "monotonic required" (durations). Go's `time.Time` has a monotonic component by default, so subtraction via `time.Since` / `a.Sub(b)` is safe; direct `time.Now().Unix()` comparisons are not.

**Test pattern.** Inject a fake clock via a package-level `var nowFunc = time.Now` override; freeze and advance deterministically.

**Acceptance.** Every duration measurement uses `time.Since` or subtraction of `time.Time` values; no `.Unix()` comparisons for durations.

#### H34: Environment variable audit

**Goal.** Find `os.Getenv` calls that silently accept an empty value as a valid default.

**Mesh sites.** `internal/config/` expands `${VAR}` in config via `os.ExpandEnv`; `internal/gateway/` `APIKeyEnv` resolves the upstream API key at request time.

**Methodology.** Skill §XII.2 methodology. Grep for `os.Getenv`. For each, trace what happens when the variable is empty: is it an error? Does the program continue with zero state? Is the user notified?

**Test pattern.** Unit test that runs the code path with the env var missing; assert a clear error either at startup or on first use.

**Acceptance.** Every required env var fails fast with a clear error; every optional one documents its default.

---

## Parked

| ID   | Component | Item                         | Notes |
|------|-----------|------------------------------|-------|
| [F3](#f3-ssh-client-subcommands) | cli | SSH client subcommands | Ad-hoc `mesh ssh user@host`. Needs terminal raw mode, SIGWINCH. |
| [F4](#f4-user-switching-setuidsetgid) | sshd | User switching | `setuid`/`setgid` (Unix), `CreateProcessAsUser` (Windows). Root required. |
| [F1](#f1-config-hot-reload) | core | Config hot-reload | File watcher, config diff, per-component context cancellation. |
| [F11](#f11-x11-forwarding) | sshd | X11 forwarding | Xauth, Unix socket, channel multiplex. Low demand. |
| [R6](#r6-homebrew-formula) | release | Homebrew formula | |
| [R7](#r7-dockerfile) | release | Dockerfile | Multi-stage build, scratch runtime. |

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

## Other Notes

- Auto load .env file from the current directory to load environment variables securely.
