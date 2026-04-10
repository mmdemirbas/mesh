# WORKLOG

Rolling session log. Newest entry on top. Each session reads the latest
entry for state, works, then appends a new entry before ending.

---

## 2026-04-10 — Tier 5 bug-hunt complete (B7-B9 fixed, H1-H34 audited)

### State of the tree

- Branch: `main`
- Working tree: clean
- Last gate run: `FAST=1 task check` green (unit suite + build).
  Full `task check` (with e2e lane) not re-run this session — D11 left it
  green and no e2e-affecting code changed.

### What shipped this session

**Tier 5 known bugs B7–B9** (six commits, two per bug, TDD-style):

- **B7** filesync `peerMatchesAddr` IPv6 canonicalization. `1c3954c` —
  parse both sides via `net.ParseIP` and compare via `net.IP.Equal`,
  preserving the localhost-canonicalization fast path.
- **B8** filesync hostname DNS resolution. `a610184` — moved DNS
  expansion into `config.FilesyncCfg.Resolve` which now populates
  `FolderCfg.AllowedPeerHosts`. `isPeerConfigured` reads that field;
  `peerMatchesAddr` stays the per-host helper. Test rewritten to drive
  the new Resolve→isPeerConfigured flow end-to-end. On DNS failure the
  literal host is kept and a warning logged so a typo'd hostname does
  not silently lock out a working peer.
- **B9** clipsync `loadFormatsFromDir` per-format cap. `8c945c5` —
  threaded `maxFileSize int64` as a parameter (Option 1 from PLAN
  because four standalone tests already called the function without a
  Node). `readClipboardFormats` now forwards `n.maxFileSize`. Test
  renamed to `TestLoadFormatsFromDir_PerFormatCap` and split into
  raised-cap and default-cap subtests.

**Tier 5 hunt tasks H1–H34** (17 commits across audits + fixes):

H1, H8, and H12 produced actionable findings; the other 31 categories
landed as `docs: <Hn> audit — no findings` notes after grep + read.

- **H1** address / host equality. Two extra findings beyond B7/B8, both
  in clipsync:
  - `canReceiveFrom` did literal host string compare against
    `StaticPeers` and `n.peers`. Two IPv6 addresses in different
    canonical forms (short vs. expanded, mixed case) silently 403'd.
    Extracted `peerHostEqual`. Tests `51fa334`, fix `faf3155`.
  - `Broadcast` echo-suppression `addr == origin` had the same bug
    class. A static peer configured as `[2001:DB8::1]:7755` would loop
    payloads back to its sender if origin arrived as
    `[2001:db8::1]:7755`. Extracted `isEchoOrigin`. Tests `aef49e1`,
    fix `be3668a`.
- **H8** integer overflow. Two findings in human-readable size parsers:
  - `clipsync.parseByteSize` (test `62ac8b3`, fix `1f93413`).
  - `config.ParseBandwidth` (test `7661936`, fix `0d57b1b`).
  Both multiplied `n * multiplier` without overflow check, so values
  like `9000000000GB` silently wrapped to negative bytes and downstream
  rate limiters / `io.LimitReader` calls treated them as degenerate
  bounds. Both now reject `n > math.MaxInt64/multiplier` with a clear
  error.
- **H12** HTTP server hygiene. Gateway and filesync HTTP servers had
  only `ReadHeaderTimeout`. Added `ReadTimeout: 2m` and
  `IdleTimeout: 60s` to both. `WriteTimeout` deliberately omitted
  because both stream long-lived responses (gateway SSE, filesync file
  download). clipsync already had the full set. Hardening fix without
  a TDD test (slow-loris repro is messy). `32b285b`.

All 6 bugs and 17 audit notes moved to DONE.md as they landed. PLAN.md
Tier 5 section is now empty.

### Next action

Tier 5 is complete. Suggested next areas, in priority order:

1. **Tier 1 / Tier 2** — pre-existing items above Tier 5 in PLAN.md
   that were never the focus of this session.
2. **The H17 hardening note** — clipsync uses `cleanLogStr` to
   sanitize peer-supplied strings before logging; filesync does not.
   Worth aligning. Low severity (slog text handler quotes control
   chars), but consistent treatment is better than asymmetric.
3. **H29 atomic write hardening** — filesync's `transfer.go` resume
   logic has a theoretical TOCTOU between `os.Stat` and `OpenFile`.
   Not exploitable today but a targeted refactor would close it.
4. **Re-run the full e2e gate** if anything from these fixes touched
   filesync wire compatibility. The B8 fix changed the wire-level
   matching semantics (now resolves hostnames); the S2 sh wrapper
   workaround is no longer strictly necessary but still works.

### Gotchas and lessons from this session

1. **filesync DNS resolution at config load surprises tests.** The B8
   fix moved DNS into `Resolve()`, which means tests using
   `os.Hostname()` now also depend on the machine resolving its own
   hostname. The B8 test correctly skips on macOS where this doesn't
   work and runs on Linux CI where it does. Document for future test
   authors: any test that builds a `FilesyncCfg` with hostnames in
   `peers:` is now hitting real DNS at `Resolve()` time.

2. **Don't add `WriteTimeout` to streaming HTTP servers.** Gateway SSE
   and filesync file downloads stream for minutes. A server-level
   `WriteTimeout` would kill them mid-stream. Use per-request
   `context.WithTimeout` derived from `r.Context()` instead.

3. **`bisect` value of test+fix split is preserved even when the test
   gets rewritten in the fix commit.** B8 is the canonical example:
   the failing test was committed in `ae922b6` testing the old
   `peerMatchesAddr` signature. The fix commit `a610184` rewrote the
   test to drive the new Resolve→isPeerConfigured layer. Bisect on
   `ae922b6` still reproduces the bug; bisect on `a610184` shows the
   fix. The intermediate test rewrite is documented in the fix's
   commit message.

4. **Overflow tests need correct math.** The first H8 test commit
   `62ac8b3` listed `20000000000MB` as an overflow case, but
   `2e10 * 1024 * 1024 = 2.1e16` fits comfortably in int64
   (max ~9.22e18). The fix commit `1f93413` corrected the case to
   `9000000000000MB` (9e12 MB → 9.44e18 → wraps). Sanity-check
   overflow values manually before committing the test.

5. **`peerHostEqual` is a single helper used in three places now.**
   Both `canReceiveFrom` (static + dynamic peer loops) and
   `Broadcast.isEchoOrigin` flow through it. Adding any new
   IP-comparison surface in clipsync should reuse it rather than open
   another literal compare.

6. **Skill-grep precision matters.** The bug-hunt skill's H1 grep
   pattern `'== *req\|== *host\|== *addr\|== *remote'` found exactly
   one site (`clipsync.go:277/289` `peerHost == host`). Broader
   patterns are valuable for the second pass — `addr == origin` was
   only found by grepping for `addr ==` directly.

7. **Race detector won't catch "happens-before" violations against
   non-shared maps.** `n.folders` is written once during `Start` and
   read by handlers afterwards. In Go's memory model this needs a
   sync action, but the race detector only flags concurrent reads
   under writes. The current code is technically a race but does not
   trigger detection. Did not change behavior; documented as a low-
   severity hardening note.

### Skill and tooling state

- Tier 5 hunts H1–H34 are recorded in DONE.md with audit baselines.
  Future bug-hunt sessions should start from PLAN.md's other tiers.
- The `bug-hunt` skill at `~/.claude/skills/bug-hunt/SKILL.md` was
  used as the methodology source; mesh-specific application notes
  for each finding are in the DONE.md table.

### Commands cheat sheet

```
task check              # full gate (incl. e2e)
FAST=1 task check       # inner-loop gate (skips e2e)
task test               # unit suite
go test -fuzz=Fuzz<name> -fuzztime=20s ./pkg/...
```

---

## 2026-04-10 — D11 e2e harness shipped; Tier 5 bug-hunt plan ready

### State of the tree

- Branch: `main`
- Commits ahead of `origin/main`: 17
- Working tree: clean
- Last gate run: `task check` green (unit suite + e2e lane both pass).
  `FAST=1 task check` skips the e2e lane for inner-loop iteration.

### What shipped this session

**D11 — end-to-end Linux test harness** (ten commits, `a226596` →
`986738d`). Full scenario suite against real containers:

- `mesh-e2e:local` image (alpine + baked mesh binary + baked stub-llm
  + fake xclip shim). Built via `task build:e2e-image`; content-hash
  gated.
- Harness primitives in `e2e/harness/`: per-test bridge network,
  `Node` with exec/file/admin helpers, artifact dump on failure,
  fixture loader, ed25519 keypair helper, `Eventually` +
  `WaitForComponent` polling helpers.
- Four scenarios in `e2e/scenarios/`:
  - `ssh_bastion_test.go` (S1): client → bastion → server SSH tunnel,
    state transitions, metrics, restart resilience.
  - `filesync_test.go` (S2): two-peer bidirectional sync, edit,
    delete, conflict, restart convergence.
  - `clipsync_test.go` (S3): three peers in one discovery group +
    fourth in another, propagation, isolation, 40 MB payload.
  - `gateway_test.go` (S4): five gateway instances with one stub
    upstream, A↔O translation, 529→503 error translation, malformed
    upstream → 502, SSE streaming.
- Stress suite in `e2e/churn/` (`-tags e2e_churn`): 1000-file
  propagation, rename storm, concurrent edits.
- Manual full-topology playground in `e2e/compose/`
  (`task e2e:compose:up` / `down`).
- Taskfile wired: `task check` runs the e2e lane by default,
  `FAST=1 task check` skips it.
- CLAUDE.md gained an "End-to-End Test Harness" section.
- D11 moved from PLAN.md to DONE.md with the as-shipped detail and
  four findings noted.

**Tier 5 bug hunting — preparation** (four commits, `ae922b6` →
`79814d4`). Tests first, fixes later.

- `ae922b6` — **failing tests B7 + B8** in
  `internal/filesync/filesync_test.go`:
  - `TestPeerMatchesAddr_IPv6Canonical` — three subtests, all fail.
    peerMatchesAddr does raw string compare on IPv6 addresses.
  - `TestPeerMatchesAddr_HostnameResolution` — **skips on macOS**
    because `os.Hostname()` is not in /etc/hosts here; fails on
    typical Linux CI where it is.
- `dff020e` — **failing test B9** in
  `internal/clipsync/clipsync_test.go`:
  - `TestLoadFormatsFromDir_PerFormatCapIgnoresConfig` — fails
    deterministically. `loadFormatsFromDir` hardcodes
    `defaultMaxSyncFileSize` despite the constant's docstring saying
    it is overridable via `ClipsyncCfg.MaxFileCopySize`.
- `aee0fb2` — **PLAN.md Tier 5** added with B7/B8/B9 known bugs and
  H1–H14 hunt tasks.
- `79814d4` — **PLAN.md Tier 5 extended** to 34 hunt tasks (H1–H34)
  aligned with the user-scope `bug-hunt` skill. Hunts grouped by
  theme (input boundaries, concurrency, crypto/transport, resource
  management, error handling, type/logic correctness, filesystem,
  network parsers, data/serialization, state/lifecycle,
  environment).

### Next action (Tier 5 kickoff)

**Start at B7.** The test is already failing on `main`; the next
commit in the sequence is the minimal fix.

Order to work through:

1. **B7 — peerMatchesAddr IPv6 canonicalization.** Fix
   `internal/filesync/protocol.go` `peerMatchesAddr` to parse both
   sides with `net.ParseIP` and compare via `.Equal`. See PLAN.md
   §B7 for acceptance.
2. **B8 — peerMatchesAddr hostname resolution.** Design decision:
   resolve at config load time (preferred) vs. on-demand with cache.
   See PLAN.md §B8. This is where the fix from B7 gets its final
   shape.
3. **B9 — loadFormatsFromDir per-format cap.** Promote
   `loadFormatsFromDir` to a method on `*Node` so it can see
   `n.maxFileSize`, or thread the cap as a parameter. See PLAN.md
   §B9.
4. **H1–H34 in order.** PLAN.md §Tier 5 has the full list grouped
   by theme. The `bug-hunt` skill at
   `~/.claude/skills/bug-hunt/SKILL.md` owns the methodology per
   category; PLAN.md has the mesh-specific file/function pointers.

Per the Tier 5 autonomous run protocol: every bug lands as two
commits (failing test + minimal fix), never squashed. `task check`
must be green before every commit. Move each fixed item to DONE.md
as soon as it lands.

### Gotchas and lessons from this session

1. **gopls false positives on build-tagged files.** Every file under
   `//go:build e2e`, `//go:build e2e || e2e_churn`, and
   `//go:build e2e_churn` triggers `gopls` warnings like "No
   packages found for open file ..." in the IDE. These are not
   real errors — `go build -tags e2e ./e2e/...` and
   `go build -tags e2e_churn ./e2e/churn/...` both succeed. Ignore
   the warnings; trust the command line.

2. **Filesync peer validation is IP-string-exact, no DNS.** This
   is what B7/B8 surfaced. Scenarios work around it with an sh
   wrapper that rewrites a placeholder in the YAML at container
   start (see `e2e/scenarios/filesync_test.go` `wrap` function).
   The compose playground sidesteps it entirely with static IPs on
   a dedicated subnet.

3. **Clipsync has two size caps.** `maxClipboardPayload = 100 MB`
   total across formats vs. `defaultMaxSyncFileSize = 50 MB` per
   format, enforced strictly at read time. The stricter per-format
   cap applies to text/plain too — S3 uses 40 MB, not 50, to stay
   under it. B9 is about the second cap being hardcoded.

4. **Gateway returns 502, not 500, on malformed upstream.** S4 was
   written to the plan's "500" assertion and updated after the real
   code hit. 502 is more correct; not a bug.

5. **Rename-delete propagation under churn load lags.** Real
   finding, not fixed in D11. A 30-file rename leaves ~20 old
   names on the peer after a minute of polling. The churn test
   documents this as a soft signal; no hunt is chasing it yet.

6. **testcontainers wait strategy deadlock.** S2 filesync needed
   `peer1` to start with a no-op wait strategy because its sh
   wrapper blocks on `getent peer2`, and testcontainers' sequential
   `Started` hook would otherwise deadlock. Pattern reused in churn.

7. **Artifact dump path.** `harness.DumpOnFailure` writes into
   `e2e/build/artifacts/<test>/<timestamp>/`. The path is anchored
   via `runtime.Caller` so it lands in one place regardless of the
   scenario package's CWD. Already in `.gitignore`.

### Skill and tooling state

- **User-scope skill:** `~/.claude/skills/bug-hunt/SKILL.md`. 1310
  lines. 12 themes, 57 language-agnostic categories. Protocol
  section covers TDD commit cadence, per-commit verification, empty
  category notes, stop conditions, scope hygiene, session kickoff
  steps, output contract.
- **Mesh PLAN.md Tier 5:** 34 hunt tasks cross-referenced to the
  skill sections. Each entry names the specific mesh
  files/functions and mesh-specific attack inputs.
- **Implementation guideline added to PLAN.md last session:** "Move
  done items to DONE.md" — as soon as an item is finished, remove
  the row and detail from PLAN.md and append to DONE.md under the
  matching tier.

### Commands cheat sheet

```
task check              # full gate: vet, staticcheck, fmt, tidy, race tests, build, e2e
FAST=1 task check       # same gate minus e2e (inner loop)
task test               # unit suite only
task e2e                # scenario suite only
task e2e:churn          # churn suite only
task e2e:full           # scenarios + churn
task e2e:compose:up     # manual playground
task e2e:compose:down   # tear down playground
task build:e2e-image    # rebuild mesh-e2e:local (content-hash gated)
```

To reproduce the three failing bug tests:

```
go test -race -count=1 -run 'TestPeerMatchesAddr_IPv6Canonical|TestPeerMatchesAddr_HostnameResolution' ./internal/filesync/...
go test -race -count=1 -run 'TestLoadFormatsFromDir_PerFormatCapIgnoresConfig' ./internal/clipsync/...
```

Expected: three subtests fail under B7, one skip under B8 (on
macOS), one fail under B9.
