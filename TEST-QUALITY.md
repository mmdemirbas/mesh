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
| internal/tlsutil  | 85.2% | `writePEM` MkdirAll/OpenFile failure branches and `generateCustom` write-cert wrap closed in `d91a48a` (writePEM 77.8%, generateCustom 71.4%). Residual uncovered branches are rand-reader and x509.CreateCertificate failures that require injection | ~~AP-1~~ fixed in `1afb5c9`; minor: string-match on `err.Error()` in `TestClientTLS_NoPeerCert_Rejected` (needs new sentinel; deferred) | None (new code) | LOW |
| internal/config   | 86.5% | `Validate` branches at 78.4% (admin_addr loopback enforcement + filesync scan_interval/max_bandwidth/path/direction rejection closed in `9987ca1`); `Load` at 55.6% (residual is error propagation from `LoadUnvalidated`/`Validate`, exercised indirectly); boundary gaps at `LoadNodeNames`/`ResolveAllowedPeerHosts` closed in `ba06243` | None found in scan | None found (spot check: `0d57b1b` has TDD reproducer in prior `7661936`) | LOW |
| internal/filesync | 57.9% | HTTP handler rejection paths closed in `b5fde66` (/index, /file, /delta) and `4a89626` (/bundle, /status). Admin-API helpers and pending-summary aggregation closed in `ec2ca9b`: `actionName`, `buildPendingSummary`, `clonePendingSummary`, `GetConflicts`, `SetConfigFolders`, `clearConfigFolders`, `recordActivity` at 100%; `GetFolderPath` 88.9%; `GetActivities` 90%. `Start` / `syncLoop` / `syncFolder` still 0% (live loops, covered end-to-end by e2e) | None found in scan | None found on spot check (fix+test-commit pattern: `c6522c1` paired with `b4ac2cf`; `5aef218`/`9f2bf7d` ship tests inline) | LOW |
| internal/clipsync | 60.3% | All HTTP trust-boundary paths now tested against the real handlers: `/discover` (`41518a7`), and after `df08f4c` extracted the inline closures, `/sync`+`/clip`+`/files` rejection paths closed in `f6eed64`. `serveClip` 100%, `serveDiscover` 92.3%, `serveSync` 79.2%, `serveFiles` 75% | None found in scan | _TBD_ | LOW |
| internal/tunnel   | 66.3% | In-process SSH runtime harness landed in `df21edf` (`runtime_test.go`) using mesh's own `NewSSHServer` as the peer: `Run` 100%, `NewSSHClient` 100%, `runMultiplex`/`runMultiplexTarget` 100%, `buildSSHConfig` 100%, `runLocalForward` 100%, `connectSSH` 85.7%, `runSession` 80.8%, `handleTCPIPForward` 75.3%, `runRemoteForward` 73%. Earlier bites: `snapshotAuthFailuresIn`/`evictOldAuthFailuresIn` 100% in `b4b6934`; `dialerControlIPQoS` 88.9% in `517489b`. Residual 0%: `runLocalProxy`, `runRemoteProxy` (SOCKS/HTTP relay paths); `probeTarget` 56.4% (mDNS retry / cache-fallback); `runForwardSetForTarget` 57.1% (error branches) | _TBD_ | `df21edf` — harness exposed and fixed a protocol-compliance bug in `handleTCPIPForward`: forwarded-tcpip payload sent `fwdReq.BindPort` (could be 0) instead of the actual ephemeral port, making the client reject channels with "no forward for address" because its forwardList is keyed by actualPort. Regression test: `TestHandleTCPIPForward_PortZeroRoundtrip` | MEDIUM |
| internal/proxy    | 68.5% | Orchestration wrappers closed in `2a65b4f` (`RunStandaloneProxies` 85.7%, `RunStandaloneRelays` 79.6%). Core serving paths already tested | None found in scan | _TBD_ | LOW |
| internal/gateway  | 84.7% | _Strong — 245 tests covering a2o/o2a translation + passthrough + streaming_. `AuditDirs`/`registerAuditDir`/`unregisterAuditDir` closed in `c6f07e5` (100%); `expandHome` 25% -> 87.5% | _TBD_ | _TBD_ | LOW |
| internal/netutil  | 98.1% | `ListenReusable` closed in `a41ddaf` (accept round-trip + empty-address wildcard bind) | None found in scan | None found | LOW |
| internal/state    | 94.1% | Previously-0% exported Metrics/State setters and `SnapshotFull` closed in `7d6d926`; `StartEviction` still 0% but is a thin ticker wrapper around the already-tested `evictStale` | None found in scan | _TBD_ | LOW |
| cmd/mesh          | 57.5% | Pidfile lifecycle and `resolveNodesForDown` closed in `ebef6b3`. Residual 0% is interactive command entry points (`runDashboard`, `upCmd`, `downCmd`, `statusCmd`, `configCmd`, `initCmd`, `completionCmd`, `main`) — covered end-to-end by `e2e/scenarios` | None found in scan | _TBD_ | LOW |
| e2e/scenarios     | _N/A_ (tests themselves) | | | | |

### Phase 4 orientation (other packages)

Cursory coverage numbers only — no full rubric pass yet. Left for
future audit cycles.

- **tunnel (37.7%)**: largest known gap. Quick-win helpers closed:
  auth-failure snapshot and eviction (`b4b6934`, 100%) and the unix
  `dialerControlIPQoS` wrapper (`517489b`, 88.9%). Every core SSH
  runtime path is still at 0% — `Run`, `NewSSHClient`, `runMultiplex`,
  `buildSSHConfig`, `runForwardSet`, `runRemoteForward`,
  `runLocalForward`, `handleTCPIPForward`, `handleDirectTCPIP`,
  `connectSSH`, `runSession`. Unit coverage of these paths requires an
  in-process SSH server over `net.Pipe()` or real TCP, plus
  auth-material setup. This is a multi-day effort — formerly PLAN.md
  D3.
- **gateway (84.1%)**: 242 tests, strongest unit suite in the repo.
  Likely in good shape; a focused audit can confirm.
- **netutil (98.1%)**: audit closed in `a41ddaf`. `ListenReusable`
  was the only 0% function; now exercised via a real accept and the
  empty-address defaulting branch.
- **proxy (68.5%)**: both orchestration wrappers now covered end-to-end
  (`2a65b4f`). Residual ~30% is defensive error branches in serving
  paths.
- **clipsync (60.3%)**: shadow-mux gap closed. The inline /sync, /clip,
  /files handlers were extracted to `serveSync`/`serveClip`/`serveFiles`
  in `df08f4c`, then exercised via `httptest.NewRecorder` in `f6eed64`
  for ACL/malformed/gzip/oversized rejection plus happy paths. The
  legacy shadow mux in `newTestNode` still exists (used by many older
  tests) but no longer holds uniquely-important coverage.
- **state (94.1%)**: tests for all previously-0% exported functions
  landed in `7d6d926`. Remaining 0% is `StartEviction`, a 3-line
  ticker wrapper around the tested `evictStale`.
- **cmd/mesh (57.5%)**: pidfile helpers and `resolveNodesForDown`
  closed in `ebef6b3`. The remaining 0% is interactive command entry
  points (dashboard, up/down/status/config, signal handlers) — covered
  end-to-end by `e2e/scenarios/`.

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
