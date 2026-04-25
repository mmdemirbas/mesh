# Plan — Workstream C: token optimization (Phase 1b)

Status: STUB. Date: 2026-04-25. Companion to MESH_VISION,
PLAN_WORKSTREAM_A_resilience, PLAN_WORKSTREAM_B_observability.

## 1. Goal

Reduce upstream input bytes when, and only when, Phase 1a and
Workstream B telemetry show a specific shape worth attacking.
This document deliberately stops short of a concrete plan: the
plan is a function of data we do not yet have.

## 2. Why this is a stub

The gateway promise is "data decides". Writing a Phase 1b spec
before the data lands would commit to a rule (delta on re-read,
tool-result truncation, system trim) on intuition — exactly the
trap the Phase 1a decision branches were drafted to avoid.

The data needed:

1. **At least one to two weeks of post-Workstream-B traffic.**
   Item B1's timing breakdown and item B5's `cache_control`
   hit rate are first-class signals; without them, Phase 1b
   decisions miss two of the four levers.
2. **Audit logs at level `full`** so re-read distributions and
   section distributions can be measured directly, not
   estimated.
3. **A traffic mix representative of normal use.** Spot-reading
   one busy day and projecting from it produces the same
   misjudgments a forecast would.

Until those three are in hand, this workstream stays a stub.

## 3. The decision tree

When the data is in, walk the branches in order:

| Observed shape | Phase 1b action |
|---|---|
| `tool_results` dominates AND `repeat_reads.count` is high | Deterministic tool-result truncation with a delta mode for re-reads. Narrow the scope to the file-read tool first. |
| `system + tools` dominates | System / tool-schema trimming — almost certainly out of scope for the gateway, since that is a client-side concern. Document the finding; do nothing. |
| Requests rarely approach the configured `context_window` | Phase 1b ships nothing. Phase 2 summarizer improvements move to the top. |
| `thinking` blocks are large | Drop them at request time for upstreams that do not support extended thinking. |
| Always | Estimator recalibration — sample around 100 real requests, compute `estimate / usage.input_tokens`, and tune `bytesPerToken` if the median lands outside [0.95, 1.25]. |

The branches are not mutually exclusive. The data may show two
or three apply, in which case rank by potential bytes saved per
implementation hour and ship one at a time.

## 4. What's already informed by data we have

A handful of decisions are already fixed by the live-traffic
sampling done during Phase 1a:

- **Request-hash caching is rejected.** Modern LLM clients
  with growing conversation history rarely produce identical
  requests within a session. The cache hit rate would be
  effectively zero. The Anthropic `cache_control` primitive
  (server-side prompt caching) is the right tool for the
  prompt-prefix caching problem; PLAN_WORKSTREAM_B item B5
  surfaces visibility into it. No transformation needed.
- **Bash-output compression is deferred.** Format drift across
  tool versions, compound commands mixing formats, and
  credential preservation expressed as a regex are all reasons
  this is safer client-side than as a gateway rule. Re-evaluate
  only if shell tool results are the dominant offender per
  Phase 1a data.
- **Session-cached summarizer is deferred.** The single-flight
  de-duplication is the v1; cross-session reuse requires a
  per-key TTL store and stronger invariants on prefix equality.
  Wait for measured pain.
- **Symbol indexing or full-text search is rejected.** Wrong
  layer (the gateway is HTTP-only, not workspace-aware) and
  would require an MCP server expansion the gateway does not
  pursue.

## 5. Dependencies

- **Workstream B must ship first.** Item B1 (timing) and item
  B5 (`cache_control` hit rate) are inputs to the decision
  tree. Without B1, "is the operator blocked on translation or
  upstream" cannot be answered. Without B5, "is `cache_control`
  already doing the work the transformation would do" cannot be
  answered.
- **At minimum one to two weeks of B-instrumented traffic.**
  Less than that risks acting on a single anomalous day.
- **Phase 1a must remain stable.** No further changes to the
  audit-row partition while the data accumulates; the analyses
  depend on a fixed schema.

## 6. What this stub will become

When the criteria in section 2 are met, this document becomes a
full plan with the same structure as PLAN_WORKSTREAM_A and
PLAN_WORKSTREAM_B:

- Goal (specific to the chosen branch).
- Scope (in / out).
- Component sketch with config additions.
- Audit row additions (likely a `phase1b_transform.applied`
  field naming what the gateway did and how many bytes it
  removed).
- Implementation sequence (commits, sized to the per-commit
  budget rule).
- Acceptance criteria including a byte-identical-on-shadow
  test to prove transformation is opt-in and faithful when
  off.
- Open questions and defaults.

The transformation must be auditable: every byte the gateway
removes is logged in the audit row, and a "diff against
untransformed" view (a sub-feature of PLAN_WORKSTREAM_B item
B6 once it exists) is the ground-truth check.

## 7. Anti-pattern guardrails

When the time comes to write the full plan, hold the line on
these:

- **Lossy transformations are explicit, not silent.** The
  operator must be able to see exactly what was removed in the
  audit row and the UI. No "trust me, it was unimportant"
  semantic edits.
- **Failure mode opens, not fails.** If the transformation hits
  a case it does not understand (unrecognized tool, novel
  block shape), pass through unmodified. Never block a request
  because the optimizer is unsure.
- **One transformation at a time.** Compose carefully. Two
  lossy layers operating in sequence make incident debugging
  ("why did the model forget X?") much harder. Add the second
  rule only after the first is well-understood in production.
- **Opt-in by config.** No optimization runs by default in v1.
  The operator enables a specific rule per upstream after
  seeing what its audit row says about expected savings.
- **No persistent per-user databases.** State stays
  in-process with a bounded TTL (matching the gateway's
  existing cache discipline). If a rule needs more state than
  that, it probably belongs in a client-side tool, not in the
  gateway.

## 8. When to revisit

Earliest meaningful date: two weeks after PLAN_WORKSTREAM_B
item B1 ships and audit-log level `full` is the steady-state
on the primary gateway. Concretely, a calendar reminder lands
at +14 days from B1's commit.

If the data at that point shows no shape worth attacking, this
document closes as "no Phase 1b shipped — telemetry instead",
which is a legitimate outcome.
