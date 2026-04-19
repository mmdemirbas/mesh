# Test Quality — Rubric and Audit

This document owns test-quality improvements across the repo. PLAN.md links
here; piecemeal entries (D3, D15, D16) are subsumed by the audit below.

## Why this exists

Two concerns drove this:

1. **Test code appears to grow slower than prod code** — a proxy for
   under-testing.
2. **Test quality is uneven** — some tests reimplement production logic,
   compute expected values rather than hardcode them, or are hard to read.

Both are real. The naive fix ("raise test LOC, write simple tests") is a
trap: LOC ratios don't measure coverage of actual behavior, and "simple"
tests that reimplement the SUT pass silently when both drift together.
This document reframes the goal in terms that are actually enforceable.

## The three rules

All three must hold for a change to be considered tested. Any one missing
is a gap, regardless of what the other two look like.

### Rule 1 — Every trust boundary has contract tests

A **trust boundary** is any interface where input comes from somewhere we
don't control: HTTP handlers, config loaders, YAML/protobuf decoders, CLI
parsers, filesystem readers that process user paths, network readers.

Each boundary needs at least:
- **Happy path** — a realistic valid input produces the expected output.
- **Rejection path** — at least one invalid input is rejected with a
  useful error (not a panic, not a silent default).
- **Edge path** — the interesting corner that was surprising the first
  time someone hit it (empty, zero, max, unicode, symlink, `../`, etc.).

Internal helpers below the boundary don't need their own contract tests
unless they encode non-trivial logic — the boundary test exercises them.

### Rule 2 — Every fixed bug ships with a reproducer

Every bug fix commit includes a test that **fails without the fix and
passes with it**. Not a test that "covers the area" — a test that, if
reverted, flips red.

This is the single most effective regression defense we have and the
cheapest to enforce: one commit, two files.

### Rule 3 — Coverage is measured by mutation, not lines

Line coverage over-reports: a test that calls `Resolve()` and asserts
nothing covers 100% of `Resolve()` but catches no bugs. Mutation testing
(flip a `>` to `>=`, swap `&&` for `||`, change a constant) asks "does
any test fail?" — if not, the lines were executed but not *tested*.

We don't need a mutation tool running in CI. We need the mindset during
review: "if I corrupted this line, would any test catch it?"

## Quality principles

These support the three rules. They are **not** goals in themselves.

### Hardcode expected values; don't compute them

```go
// Bad — reimplements the SUT
want := strings.ToUpper(input)
got := Normalize(input)

// Good — pinned value
got := Normalize("hello")
require.Equal(t, "HELLO", got)
```

Tests that compute expectations via prod code pass when both are equally
wrong. Hardcoded values force you to know what "correct" actually means.

### Oracle exception: explicit and isolated

Some invariants are only expressible via the production function itself:
"fingerprint of this cert is whatever `tlsutil.Fingerprint` returns for
it." Calling `Fingerprint` in a test that verifies *pinning behavior* is
fine — the test pins the check, not the compute.

But: **`Fingerprint` itself needs at least one test with a hardcoded
expected hex string computed out-of-band**. Without that, the oracle has
no anchor and any bug in `Fingerprint` is invisible.

This applies anywhere a test uses prod code to produce "expected" values:
the deeper helper needs a separate, hardcoded-value test.

### Delete bad tests

A test that asserts nothing, a test that tests the mock, a test that
has been `t.Skip`'d for more than one release — delete it. Keeping
noise normalizes noise. A smaller suite of real tests beats a larger
suite with decorative entries.

### Test helpers have contracts, not options

When test code repeats, **first simplify the tests**. If it's still
repetitive, extract a helper with:
- A clear single purpose.
- No flags that change semantics (no `helper(t, withAuth bool, strict
  bool)` — write two helpers).
- Enough assertions inside that a caller doesn't re-assert the same
  invariants.

A helper that takes five booleans is a framework. We don't need one.

### Integration complexity is real; containerized complexity is necessary

E2E tests are harder to write than unit tests. That's the price of
verifying real wire behavior. The response is not "simplify by mocking
the network" — it's "invest in harness primitives that make the real
integration readable." `harness.StartNode`, `harness.Eventually`, and
`NodeOptions.Files` exist for this.

Unit-test simplicity rules apply inside unit tests. E2E readability is
about the harness, not about the test.

## Audit methodology

Per-package, producing a punch list:

1. **Inventory trust boundaries.** List every exported entry point that
   takes external input. For each: does it have happy/reject/edge
   contract tests?
2. **Scan for anti-patterns** (see catalog below). One grep per pattern,
   each hit is a line item.
3. **Count mutation-resistance.** For each significant test, ask: "if I
   broke line X, would this fail?" Spot-check; don't run a tool.
4. **Inventory fixed bugs without reproducers.** `git log --grep=fix` and
   cross-reference against tests added in the same commit.

Output is a table per package: `boundary gaps | anti-patterns found |
reproducer gaps | priority`. Priorities come from Rule 1 (boundaries
first, internal helpers later).

The goal is not uniform coverage. It's: every trust boundary has
contract tests; every shipped fix has a reproducer; no tests reimplement
the SUT.

## Per-package audit snapshot

Line coverage is shown for orientation only. Priority is driven by
Rule 1 (boundary gaps) and Rule 2 (reproducer gaps), not the number.

| Package | Line cov | Boundary gaps | Anti-patterns | Reproducer gaps | Priority |
|---------|----------|---------------|---------------|-----------------|----------|
| internal/tlsutil  | 79.6% | `generateCustom` edge branches untested; `writePEM` 55.6% | ~~AP-1~~ fixed in `1afb5c9`; minor: string-match on `err.Error()` in `TestClientTLS_NoPeerCert_Rejected` (needs new sentinel; deferred) | None (new code) | MED |
| internal/config   | 83.3% | `Validate` branches at 62.2%; `Load` at 55.6% (boundary gap at `LoadNodeNames`/`ResolveAllowedPeerHosts` closed in `ba06243`) | None found in scan | None found (spot check: `0d57b1b` has TDD reproducer in prior `7661936`) | MED |
| internal/filesync | 58.6% | HTTP handler rejection paths partly closed in `b5fde66` (method/protobuf/gzip on index,file,delta); `handleBundle` / `handleStatus` rejection paths still uncovered; `Start` / `syncLoop` / `syncFolder` 0% | None found in scan | None found on spot check (fix+test-commit pattern: `c6522c1` paired with `b4ac2cf`; `5aef218`/`9f2bf7d` ship tests inline) | MED |
| internal/clipsync | 55.9% | _TBD — 67 tests, needs gap analysis_ | _TBD_ | _TBD_ | LOW |
| internal/tunnel   | 36.4% | Core SSH paths at 0%: `Run`, `NewSSHClient`, `runMultiplex`, `buildSSHConfig`, `runForwardSet`, `runRemoteForward`, `runLocalForward`, `handleTCPIPForward`, `handleDirectTCPIP`, `connectSSH`, `runSession` | _TBD_ | _TBD_ | HIGH |
| internal/proxy    | 46.3% | Only `RunStandaloneProxies` and `RunStandaloneRelays` (orchestration wrappers) uncovered; core serving paths tested | None found in scan | _TBD_ | LOW |
| internal/gateway  | 84.1% | _Strong — 242 tests covering a2o/o2a translation + passthrough + streaming_ | _TBD_ | _TBD_ | LOW |
| internal/netutil  | 81.5% | _TBD — small package, 10 tests_ | _TBD_ | _TBD_ | LOW |
| internal/state    | 54.6% | _TBD — 27 tests; Global/Snapshot surface needs review_ | _TBD_ | _TBD_ | MED |
| cmd/mesh          | _TBD_ | | | | _TBD_ |
| e2e/scenarios     | _N/A_ (tests themselves) | | | | |

### Phase 4 orientation (other packages)

Cursory coverage numbers only — no full rubric pass yet. Left for
future audit cycles.

- **tunnel (36.4%)**: largest known gap. Every core SSH runtime path
  is at 0% — `Run`, `NewSSHClient`, `runMultiplex`, `buildSSHConfig`,
  `runForwardSet`, `runRemoteForward`, `runLocalForward`,
  `handleTCPIPForward`, `handleDirectTCPIP`, `connectSSH`,
  `runSession`. Unit coverage requires an in-process SSH server over
  `net.Pipe()` or real TCP, plus auth-material setup. This is a
  multi-day effort — formerly PLAN.md D3.
- **gateway (84.1%)**: 242 tests, strongest unit suite in the repo.
  Likely in good shape; a focused audit can confirm.
- **netutil (81.5%)**: small helper package, likely well-tested.
- **proxy (46.3%)**: misleading number — only `RunStandaloneProxies`
  and `RunStandaloneRelays` orchestration wrappers are at 0%. SOCKS5
  and HTTP CONNECT serving paths are well-tested.
- **clipsync (55.9%)**, **state (54.6%)**: medium gaps, worth an
  audit pass before promoting beyond LOW/MED.
- **cmd/mesh**: not measured — mostly wiring and CLI shells; integration
  tests in `e2e/scenarios/` are the natural coverage.

### Phase 2 notes

- **tlsutil**: all twelve existing tests are well-written (no shared
  state, `t.Parallel`, deterministic). The weakness is the oracle —
  every fingerprint assertion is relative (`fp1 == fp2`), shape-only
  (prefix + length), or compared against a value computed from the
  same cert. Swapping SHA-256 for SHA-1 in `Fingerprint` would pass
  every existing test. AP-1 fix: embed a static PEM fixture in
  `testdata/` with a known-out-of-band fingerprint.
- **config**: `config_test.go` (40 tests) uses good hardcoded-value
  patterns and table-driven rejection cases. The two 0% exported
  functions are the clearest gap — both are trust boundaries (node-
  name loader and peer-host resolver) with zero contract tests.
- **filesync**: 180 tests, 180 KB of test code — the package is not
  under-tested by volume. The gap is shape. HTTP handler code is
  exercised only via e2e, not via unit-level contract tests. A
  malformed protobuf, oversize payload, or unauthorized peer produces
  behavior currently verified only by running containers.
- **Rule 2 practice (positive finding).** Spot checks across five
  fix commits found the repo consistently pairs fixes with tests:
  inline in the same commit (`5aef218`, `9f2bf7d`) or in an adjacent
  commit titled `test(...)` or `add tests for ...` (`0d57b1b` ↔
  `7661936`; `c6522c1` ↔ `b4ac2cf`). This is the practice to preserve
  in rollout; the three TEST-QUALITY rules codify what is already
  happening, not something new.

## Known anti-patterns in this repo

Concrete instances, disclosed honestly. When fixed, delete the entry
and commit the fix.

### AP-1: TLS fingerprint e2e uses prod code as its oracle

**Where:** `e2e/scenarios/tls_test.go::generateTLSMaterial` calls
`tlsutil.AutoCert` and uses the returned fingerprint as the "expected"
value pinned into peer configs.

**Why it's weak:** The test verifies *that pinning works* (good) but its
notion of "the right fingerprint" is "whatever `AutoCert` produced"
(oracle). A bug in `Fingerprint` would pass the e2e unchanged.

**Fix:** `TestFingerprint_PinnedPEM` in `internal/tlsutil/tlsutil_test.go`
anchors the algorithm against a static PEM fixture and a hardcoded hex
computed out-of-band with `openssl x509 -noout -fingerprint -sha256`.
The e2e continues to use `AutoCert` for pinning-behavior tests, which
is the correct concern there.

**Status:** Fixed in `1afb5c9` (2026-04-19).

### AP-2: Piecemeal coverage goals without rubric

**Where:** PLAN.md previously tracked "tunnel coverage at 34%" (D3) and
"no passthrough e2e" (D15) as isolated items.

**Why it's weak:** Both are symptoms, not a diagnosis. Raising tunnel
line coverage to 80% doesn't mean `tunnel`'s boundaries are tested —
and passthrough e2e alone doesn't address the broader gap that gateway
unit tests compute expectations via prod translators.

**Fix:** This document replaces those line items. D3, D15, D16 are
superseded; individual fixes land as part of per-package audit work.

**Status:** In progress (this document).

_New entries: add only from real findings. Do not pre-fill speculatively._

## Rollout

1. **Phase 1 — land this doc and the rubric.** Done.
2. **Phase 2 — audit three packages first:** `internal/config`,
   `internal/tlsutil`, `internal/filesync`. Done.
3. **Phase 3 — fix the top-priority gaps.** In progress. Closed so far:
   AP-1 (tlsutil oracle anchor); config boundary gaps on `LoadNodeNames`
   and `ResolveAllowedPeerHosts`; filesync protocol-handler rejection
   paths on `/index`, `/file`, `/delta`. Remaining: `handleBundle` and
   `handleStatus` rejection paths; deeper coverage of `Validate` and
   `Load` branches; live-loop paths (`Start`, `syncLoop`).
4. **Phase 4 — remaining packages,** same pattern: `clipsync`, `tunnel`,
   `proxy`, `gateway`, `netutil`, `state`, `cmd/mesh`, `e2e/scenarios`.

No deadline. Quality work blocks no feature; feature work does not
create new Rule-1 or Rule-2 gaps (enforced at review, not CI).
