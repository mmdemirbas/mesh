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
| P14  | tunnel | Parallel target probing (Happy Eyeballs) | Stagger-start all targets concurrently in `probeTarget` instead of sequential. First to connect wins, respecting target order preference within a short tie-break window. Reduces worst-case probe time from sum(timeouts) to max(stagger, fastest_target). Nice-to-have. |
| [P15](#p15-dns-result-caching-for-target-probes) | tunnel | DNS result caching for target probes | Cache hostname→IP from successful connections. Eliminates slow mDNS re-resolution on reconnect. Design below. |

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

#### P15: DNS result caching for target probes

**Goal:** Eliminate DNS resolution delay on reconnect by reusing the IP address from the previous successful connection. Primary motivation: Windows mDNS is slow to resolve `.local` hostnames after a network disruption — each target probe blocks for ~6s (3s dial + 150ms mDNS retry + 3s retry) before falling through.

**Approach:**

Package-level `dnsCache` in `internal/tunnel` — a `sync.RWMutex`-protected `map[string]dnsCacheEntry`.

```go
type dnsCacheEntry struct {
    ip         string
    resolvedAt time.Time
}
```

**Population:** After a successful TCP connection in `probeTarget` (failover mode) or `runForwardSetForTarget` (multiplex mode), extract the remote IP from `conn.RemoteAddr()` and store `hostname → IP`. Skip when the hostname is already an IP literal (`net.ParseIP(host) != nil`).

**Lookup:** At the start of each target dial, if the hostname has a cache entry younger than the TTL (5 min), replace the hostname with the cached IP in the dial address. The original hostname is preserved for SSH host-key verification and logging — only the TCP dial uses the cached IP.

**Retry path unchanged:** If the cached IP fails (DHCP reassigned, host unreachable), the existing `.local` retry logic fires using the original hostname, triggering fresh DNS. On success, the cache is updated with the new IP.

**Integration points:**

1. `probeTarget` (tunnel.go:1194) — failover mode. Before each `dialer.DialContext`, check cache. After successful dial, populate cache.
2. `runForwardSetForTarget` (tunnel.go:701) — multiplex mode. Same pattern. This path currently has no mDNS retry at all, so the cache is especially valuable here.

**TTL:** 5 minutes. No config knob — DHCP leases are typically 1h+ so 5 min is conservative. Entries older than TTL are ignored during lookup but not actively evicted (lazy cleanup during write).

**Performance characteristics:**

| Scenario | Latency | vs. today |
|---|---|---|
| Cache hit, IP valid | ~1ms (TCP SYN-ACK, zero DNS) | Down from 1-6s |
| Cache hit, IP stale, old host sends RST | ~50ms, then normal retry | Same |
| Cache hit, IP stale, old host unreachable | 3s timeout, then normal retry | Same |
| Cache miss (first connection, TTL expired) | Unchanged | Same |

**Key decisions:**

- Global (package-level) cache, not per-SSHClient. Hostname→IP is host-level truth, shared across connections.
- No eviction on connection failure. The IP is usually still correct when an SSH session dies — the network route is intact, just the SSH layer disconnected. TTL handles genuine IP changes.
- 5 min TTL balances freshness (DHCP churn) vs. cache effectiveness (most reconnects happen within seconds of a drop).

**Risks/dependencies:** None. The cache is a transparent optimization with no protocol or config changes. Stale entries cost at most one extra timeout (3s) before the retry path resolves correctly.

**Effort:** S — ~50 lines of new code (cache type + two integration points), table-driven tests for hit/miss/stale/IP-literal-skip scenarios.

**Adversarial review findings:**

F1 (high) — **Retry path must preserve the original hostname.** The design says "replace the hostname with the cached IP in the dial address" but doesn't specify which dial. `probeTarget` has two dials per `.local` target: the primary and the mDNS retry after 150ms. If the implementation replaces `hostPort` in place, BOTH dials use the cached IP and the retry is useless. Fix: introduce a separate `dialAddr` variable for the first attempt; keep `hostPort` unchanged for the mDNS retry path.

F2 (high) — **Stale cache causes 5-minute retry loops in multiplex mode.** `runForwardSetForTarget` has no per-attempt hostname fallback — no mDNS retry, no target list to iterate. If the cached IP is stale, it dials the wrong IP, waits `retryInterval` (10s), loops, reads the same cache entry (TTL not expired), dials the wrong IP again. This repeats for up to TTL (5 min). Fix: when a dial using a cached IP fails, delete that cache entry immediately so the next iteration falls through to DNS.

F3 (medium) — **IPv6 link-local zone IDs are unstable.** Production logs show `muhammed-mbp.local` resolving to `[fe80::421:bbe7:c5bd:2ec1%Wi-Fi]:2222`. The zone ID `%Wi-Fi` is a Windows adapter name — it becomes invalid if the adapter is renamed, disabled, or if the OS picks a different interface. Caching a link-local address with a stale zone ID produces guaranteed dial failures until TTL expires.

F4 (medium) — **Single-family IP locking.** mDNS returns both IPv4 (`192.168.68.134`) and IPv6 link-local (`fe80::...%Wi-Fi`). The cache stores whichever was used last. If the IPv6 route breaks (common: Wi-Fi adapter reset, network switch), the cache keeps trying it for up to TTL while IPv4 would connect instantly. This is the opposite of the design intent.

F3+F4 fix: skip caching when `conn.RemoteAddr()` is a link-local address (`ip.IsLinkLocalUnicast()`). Only cache IPv4 or global-scope IPv6. This avoids the zone-ID and single-family problems for the most common failure mode. If the only working route is link-local, the cache simply doesn't help and the existing DNS path handles it — no regression.

F5 (low) — **IPv4-mapped IPv6 normalization.** On dual-stack platforms, `conn.RemoteAddr()` may return `::ffff:192.168.68.134`. Normalize with `net.IP.To4()` before caching to store the canonical IPv4 form.

F6 (low) — **"At most one extra timeout" claim is imprecise.** In failover mode: 3s + 150ms (one timeout + mDNS wait). In multiplex mode without the F2 fix: up to 5 min of repeated failures. With the F2 fix: one timeout (3s) + one retry interval (10s) before DNS takes over.

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

All B7–B9 fixed. See DONE.md.

### Tier 5 Hunt Tasks

H1–H14 were the first pass surfaced directly by the D11 e2e work. H15–H30 come from a second-pass review and align with the generic bug-hunt skill (`~/.claude/skills/bug-hunt/SKILL.md`). For each hunt below, the skill entry in brackets points at the full methodology; this PLAN.md row names the mesh-specific files, functions, and attack inputs to audit.

**Input boundaries**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Cryptography and transport**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Concurrency**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Resource management**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Error handling**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Type and logic correctness**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Filesystem**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Network parsers**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Data and serialization**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**State and lifecycle**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|

**Environment and platform**

| ID   | Area                                                     | Skill § | Lens        |
|------|----------------------------------------------------------|---------|-------------|
| [H33](#h33-clock-monotonic-audit)                        | Wall-clock used where monotonic is required              | XII.1   | Grep        |

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
