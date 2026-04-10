# WORKLOG

Rolling session log. Newest entry on top. Each session reads the latest
entry for state, works, then appends a new entry before ending.

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
