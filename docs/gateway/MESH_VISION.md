# Mesh — LLM gateway scope and roadmap

Status: DRAFT. Date: 2026-04-25. Companion to
PLAN_WORKSTREAM_A_resilience, PLAN_WORKSTREAM_B_observability,
PLAN_WORKSTREAM_C_optimization.

## 1. Why this doc exists

Mesh's LLM gateway has reached a checkpoint. The audit-row schema
work (Phase 1a — telemetry only) is wrapping up. Three larger
workstreams are visible on the next horizon:

1. Multi-upstream resilience (PLAN_WORKSTREAM_A).
2. Observability and UI overhaul (PLAN_WORKSTREAM_B).
3. Token optimization, Phase 1b (PLAN_WORKSTREAM_C — data-gated).

This document fixes the framing those three depend on: the
operating context, what "done" looks like for the LLM-tooling
axis, and what is deliberately excluded.

## 2. Operating context

Mesh is a single-binary networking tool deployed on a developer's
local machines, optionally with a public bastion in between. One
mesh process per machine. No clustering. No multi-tenant rollout.

For the LLM gateway specifically:

- **Primary client surface**: an Anthropic Messages API client
  running on the operator's workstation, talking to mesh over
  loopback HTTP.
- **Primary upstreams**: a translation pair (Anthropic Messages
  API in front, OpenAI-compatible Chat Completions API behind)
  and a passthrough pair (Anthropic in, Anthropic out, used for
  direct vendor access with audit logging).
- **Topology**: mesh dials upstreams over the public internet or
  through an SSH-tunnelled HTTP proxy as needed. Admin server is
  loopback-only.

Mesh's other axes (filesync, clipsync, SSH tunnelling, SOCKS and
HTTP proxy) are mature and operate without scheduled design work.
This document constrains itself to the LLM gateway axis.

## 3. The five capability axes

A complete LLM gateway, scoped to a single-operator deployment,
covers five capability axes:

1. **Translation gateway.** Translate between the Anthropic
   Messages API and the OpenAI Chat Completions API in either
   direction; passthrough each side to itself; route by model
   pattern; remap model names per upstream. Shipped.
2. **Summarization.** Compact long histories by calling a
   summarizer upstream once a request approaches the upstream's
   context window. Shipped, with `tool_use` / `tool_result`
   pair-safety, single-flight de-duplication, and a calibrated
   token estimator.
3. **Token optimization.** Reduce upstream input bytes when the
   data shows it pays — read-delta, structural trim, tool-result
   truncation. Decided by data, not forecast. Phase 1a
   (telemetry only) is shipping; Phase 1b (transformation) is
   gated on those measurements (PLAN_WORKSTREAM_C).
4. **Observability.** Audit rows carrying section-byte
   partitions, repeat-read detection, top tool-result tracking,
   response-byte partition, stream timing, and summarize delta.
   Phase 1a is near complete on the data side; PLAN_WORKSTREAM_B
   turns those rows into a UI an operator can use to diagnose
   live traffic.
5. **Metrics and quota.** Prometheus counters per gateway; a
   read-through cache for upstream rate-limit headers; per-
   upstream and per-key health surfacing produced by
   PLAN_WORKSTREAM_A.

## 4. Status of each axis (2026-04-25)

| Axis | Status |
|------|--------|
| Translation gateway | shipped |
| Summarization | shipped; tune from Phase 1b data |
| Token optimization, telemetry (Phase 1a) | shipped piecewise; admin-UI breakdown card and per-section metrics counters pending |
| Token optimization, transformation (Phase 1b) | deferred — needs data |
| Observability, audit-row schema | shipped (Phase 1a) |
| Observability, UI | partial; full overhaul pending |
| Resilience (multi-upstream, multi-key, failover) | not started |
| Quota display | designed, not started |

## 5. Workstream order — and why

**B before A.** B (observability and UI overhaul) is loaded every
admin-UI session; the value compounds with use. A (resilience)
only matters in failure modes the operator does not currently
hit. Frequency-of-leverage wins: ship the surface that earns its
keep on every session, then build the safety net for the rare
bad day.

**C after B.** Phase 1b transformation needs at least one to two
weeks of post-B traffic before the data is interpretable. B's
timing breakdown and live session view also surface the cases C
must address — without that surface, C is forecasting.

**A in parallel or after B.** A's correctness-critical pieces
(failover, key rotation, retry safety) keep a slow design-heavy
cadence. B's UI pieces iterate faster on a separate code surface
and do not share files with A. The two streams can run
independently once B's backend is stable.

## 6. Deliberate exclusions

These are intentionally not on any plan:

- **Cross-mesh load balancing.** One mesh per machine. No
  clustering, no service mesh, no peer-aware routing.
- **Cross-region failover.** Out of scope at single-operator
  scale.
- **Quota prediction.** Mesh observes upstream-authoritative
  values from response headers and account-usage APIs. Local
  predictive accounting drifts from upstream truth and adds
  maintenance with no payoff at this scale.
- **Request replay or live editing from the UI.** The UI is
  read-only. Re-running a request is a "copy as curl" in the
  operator's terminal (delivered by PLAN_WORKSTREAM_B item B6).
- **Cost accounting in dollars.** Tokens are the operative unit
  upstream. Currency conversion depends on contract pricing
  that varies per account; computing it locally re-introduces a
  divergence source. Provider dashboards already cover the
  dollar question.
- **Generic identical-request caching.** Modern LLM clients with
  growing conversation history rarely send byte-identical
  requests within a session — every turn changes at least the
  tail of the message array. A local request-hash cache would
  see a near-zero hit rate on this workload. The Anthropic
  `cache_control` primitive (server-side prompt caching) is the
  right tool for the prompt-prefix caching problem;
  PLAN_WORKSTREAM_B item B5 surfaces visibility into it instead
  of duplicating it locally.
- **Multi-tenant deployment.** No authentication, no per-user
  quota, no rate-limit-per-key inside mesh. Loopback admin
  server, operator-managed config, single OS account.

## 7. What this means for the next stretch of work

1. **First, write the four planning docs.** This document plus
   PLAN_WORKSTREAM_A, PLAN_WORKSTREAM_B, and PLAN_WORKSTREAM_C
   land as one documentation commit.
2. **Next, implement PLAN_WORKSTREAM_B item B1 (timing
   breakdown), backend half.** Well-bounded; applies the same
   partition rigor used on the byte side of the audit row.
3. **Then proceed through B2 — B6 in some order.** UI work
   accelerates at this point; checkpoint the SPA between commits
   so each stays reviewable.
4. **PLAN_WORKSTREAM_A lands in parallel with, or after, B's
   backend is stable.**
5. **PLAN_WORKSTREAM_C only after one to two weeks of post-B
   traffic accumulates.** The decision tree lives in
   PLAN_WORKSTREAM_C section 3.

The point of pinning the framing before B1 is to preserve the
"data decides" discipline of Phase 1a. Without it, the natural
gravity of UI work is to ship features that look impressive
rather than features the data validates as worth shipping.
