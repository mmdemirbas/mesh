# Plan — Workstream A: multi-upstream resilience

Status: DRAFT. Date: 2026-04-25. Companion to MESH_VISION,
PLAN_WORKSTREAM_B_observability, PLAN_WORKSTREAM_C_optimization.

## 1. Goal

Survive single-upstream and single-key failure modes without
operator intervention. Today, when the primary upstream
rate-limits or a network path drops, the LLM client receives a
429 or 503 and the operator must wait, switch endpoints by hand,
or restart mesh. This workstream removes that manual step for
the common failure shapes.

Success criteria after Workstream A ships:

1. A rate-limited primary upstream rotates to a healthy fallback
   inside one client request, with one logged retry.
2. A single API key hitting its per-window cap rotates to
   another key on the same upstream without restarting mesh.
3. The admin UI shows which upstreams are healthy, degraded, or
   unhealthy at a glance, and why.
4. The audit row carries a complete trail: every upstream
   attempted, the one that produced the response, and the
   reason each non-final attempt failed.

## 2. Scope

### In

- Per-upstream health state with healthy / degraded / unhealthy
  transitions driven by observed traffic plus optional active
  probes.
- Multiple keys per upstream via an environment-variable list.
  LRU selection among healthy keys.
- Auto-rotation on rate-limit. Parse the upstream's
  `Retry-After` header and the provider-native `*-ratelimit-*`
  headers; mark the offending key degraded with a reset
  timestamp; retry the request once with the next available
  key (or upstream).
- Upstream chain in routing rules. A single rule lists ordered
  fallbacks; the gateway tries them in order, skipping any
  marked unhealthy.
- Audit row records `attempted_upstreams[]`, `final_upstream`,
  and `fallback_reason`.
- Admin UI surface: per-upstream health badge in the upstreams
  list, a small "rotated" indicator on requests that fell
  through.

### Out

- Cross-mesh load balancing. One mesh per machine.
- Cross-region or geo-aware failover. Not relevant at
  single-operator scale.
- Quota prediction or proactive throttling. Quota observation
  is an independent track; this workstream does not predict
  remaining quota.
- Active blackbox probes against arbitrary endpoints. The probe
  is whatever the gateway already calls — no new request shape
  is invented.
- Per-rule per-key affinity (for example, "rule X always uses
  key 2"). Keys are pooled per upstream; routing picks an
  upstream then the upstream picks a key.
- Circuit-breaker latency budgets, hedged requests, request
  shadowing. None justified at this scale.

## 3. Component sketch

### A1 — Per-upstream health state

In-process map keyed by upstream name. Each entry holds:

```go
type upstreamHealth struct {
    state             healthState // healthy | degraded | unhealthy
    consecutiveFails  int
    lastFailErr       error
    lastFailAt        time.Time
    lastSuccessAt     time.Time
    rateLimitResetAt  time.Time   // zero unless degraded by a 429
}
```

**State transitions** (driven by observed responses; A1 ships
without active probes):

- `healthy → degraded` on a single 429 with parseable reset, OR
  on three consecutive non-200 / non-429 failures.
- `healthy → unhealthy` on connection-refused / DNS / TLS
  failure (the upstream is genuinely unreachable, not just
  saturated).
- `degraded → healthy` after a successful 200, OR after the
  parsed `rateLimitResetAt` passes.
- `unhealthy → degraded` after one successful response from a
  probe (item A1b adds the probe).
- `unhealthy → healthy` after two consecutive successful
  requests.

**Counter discipline.** `consecutiveFails` resets to 0 on every
2xx; any non-2xx increments by one. The map lives in a new
`internal/gateway/health.go`, one mutex per upstream entry, no
global lock. Entry lifetime is process lifetime — restarting
mesh resets every state to healthy.

Surface to admin UI: `GET /api/gateway/upstreams` returns the
state map plus a `last_error` field with sensitive content
redacted.

**PLAN_QUOTA cross-reference.** PLAN_QUOTA.local.md item M1
covers upstream rate-limit header capture with persistent
per-gateway storage. A1's in-memory health map and PLAN_QUOTA's
`~/.mesh/gateway/<name>.quota.json` persistence overlap at the
capture step: a single header parser feeds both surfaces. Worth
consolidating during implementation.

### A1b — Active probes

Optional. Lands as a separate commit. Background goroutine per
gateway, running every 60 seconds. Iterates upstreams whose
state is `unhealthy` for more than 60 seconds and whose
`lastFailAt` is older than 30 seconds. The probe is the same
handler the gateway already calls — for an OpenAI upstream, a
minimal `/v1/chat/completions` with `max_tokens: 1`; for an
Anthropic upstream, a minimal `/v1/messages`. One probe success
transitions to `degraded`; two probe successes in a row →
`healthy`. Probe failures do not change state (already
unhealthy).

Probe responses are not translated and not recorded in the
audit log. Tag them with `X-Mesh-Probe: 1` so any observer can
filter them out.

**Cost discipline.** Probes only fire for upstreams marked
unhealthy AND in active use (referenced by at least one routing
rule). Idle upstream definitions do not generate probe traffic.

### A2 — Multi-key support per upstream

Today: `UpstreamCfg.APIKeyEnv` is a single string.

Proposed:

```yaml
upstream:
  - name: openai-primary
    target: https://api.example.com/v1/chat/completions
    api: openai
    api_key_envs:                 # NEW: ordered list
      - OPENAI_API_KEY_1
      - OPENAI_API_KEY_2
      - OPENAI_API_KEY_3
    # api_key_env (singular) still accepted; treated as
    # api_key_envs: [<value>] when present.
```

Selection at request time: iterate `api_key_envs` in order.
Pick the first key whose per-key state is healthy and whose
`rateLimitResetAt` (if set) has passed. If none match, fall
back to "least recently rate-limited" — pick the one whose
reset is closest to now.

Per-key state lives in a sibling map keyed by `(upstreamName,
keyIndex)`. Same struct as `upstreamHealth` but scoped per key.
A single key's degradation does not mark the upstream itself
as degraded — the upstream stays healthy as long as at least
one key works.

**OAuth path unchanged.** When `api_key_envs` is absent and
`api_key_env` is also absent, the gateway preserves the client's
auth headers verbatim (the OAuth-passthrough case). Multi-key
selection does not apply in this mode — there is exactly one
set of incoming auth headers per request.

**Validation.** `cfg.Validate` rejects an `api_key_envs` list
with empty strings. Environment variables are resolved at
gateway start (not at request time). A missing variable
produces a one-shot warning; the upstream remains usable as
long as at least one variable resolved.

### A3 — Auto-rotation on rate-limit

On a 429 response from an upstream:

1. Parse `Retry-After` (numeric seconds, or HTTP-date) → an
   absolute reset time.
2. If absent, parse provider-native headers:
   - Anthropic: `anthropic-ratelimit-{requests,tokens,
     input-tokens,output-tokens}-reset` (RFC 3339).
   - OpenAI / OpenAI-compatible: `x-ratelimit-reset-{requests,
     tokens}` (RFC 3339, or a duration like `1m20s`).
3. If still absent, default the reset to `now + 30s`.
4. Mark the current key degraded with the parsed reset.
5. If the same upstream has another healthy key, retry the
   request once with that key. The retry copies the original
   request body verbatim — no re-translation, no
   re-summarization.
6. If no other key on this upstream is healthy, fall through
   the upstream chain (item A4) to the next upstream.
7. If every upstream in the chain is exhausted, return 429 to
   the client with `Retry-After` set to the soonest reset
   across all attempted keys.

**Single retry, never more.** Two reasons. First, deeper retry
schemes risk user-visible latency growing without bound on a
multi-upstream chain (one retry × five upstreams = five seconds
of opaque waiting). Second, the audit row's
`attempted_upstreams[]` field stays simple to read.

**Idempotency.** The retry sends the same request bytes
upstream. Anthropic and OpenAI both treat their messages
endpoints as idempotent at the protocol level — a duplicate
upstream call costs tokens twice but produces no other side
effects. Document this in the user-facing error message.

**PLAN_QUOTA cross-reference.** A3's parsing requirements
(`Retry-After`, Anthropic and OpenAI rate-limit families) are
exactly the parser shape PLAN_QUOTA.local.md item M1 explores
for quota display. Implement once, populate both: the rotation
logic and the quota cache read the same response headers.

### A4 — Upstream chain in routing rules

Today: `RoutingRule.UpstreamName` is a single string.

Proposed:

```yaml
routing:
  - client_model: ["model-large-*", "model-xl-*"]
    upstream_chain:                      # NEW: ordered list
      - openai-primary
      - openai-fallback
      - anthropic-direct
    # upstream_name (singular) still accepted; treated as
    # upstream_chain: [<value>] when present.
```

**Selection logic.** Iterate `upstream_chain` in order. For
each upstream, check its health: skip `unhealthy`, attempt
`degraded` and `healthy` (a degraded upstream may still have
one working key). On 200 the chain is done. On rate-limit
(after the A3 retry within the upstream is exhausted), advance
to the next chain entry. On connection failure, mark the
upstream `unhealthy` and advance.

**No "active probe to choose".** The choice is "try in order,
skip unhealthy" — not "ping all and pick the lowest latency".
Adding that complexity would require a multi-second probe
budget per request, which is worse than the current default
failure mode.

**Backward compat.** `upstream_name` keeps working; the loader
populates `upstream_chain` from it.

**Validation.** Every name in `upstream_chain` must reference
a defined upstream (the same check as today, lifted to list
shape). An empty chain is a configuration error.

### A5 — Audit row fields for fallback tracking

Adds three fields to the existing `resp` row:

```json
{
  "t": "resp",
  "id": 73,
  "...": "...",
  "attempted_upstreams": [
    {
      "name": "openai-primary",
      "key_index": 0,
      "started_at": "2026-04-25T13:18:42.118+03:00",
      "ended_at":   "2026-04-25T13:18:42.221+03:00",
      "http_status": 429,
      "reason": "rate_limit",
      "rate_limit_reset_at": "2026-04-25T13:19:00+03:00"
    },
    {
      "name": "openai-primary",
      "key_index": 1,
      "started_at": "2026-04-25T13:18:42.222+03:00",
      "ended_at":   "2026-04-25T13:18:50.401+03:00",
      "http_status": 200,
      "reason": "ok"
    }
  ],
  "final_upstream": "openai-primary",
  "final_key_index": 1,
  "fallback_reason": "rate_limit"
}
```

**Field semantics.**

- `attempted_upstreams[]` — every attempt, in order. Always at
  least one entry. The last entry is the final attempt; its
  outcome decides the request status.
- `attempted_upstreams[i].reason ∈ {"ok", "rate_limit",
  "connection_failure", "timeout", "5xx", "4xx_client_error"}`.
- `final_upstream` and `final_key_index` — the name and key
  index of the entry that produced the response, OR empty if
  every attempt failed.
- `fallback_reason` — why the request fell through to a
  non-first entry. Empty when only one attempt happened.
  Possible values: `"rate_limit"`, `"upstream_unhealthy"`,
  `"connection_failure"`, `"timeout"`, `"5xx"`. Mirrors the
  `reason` of the last non-final attempt for at-a-glance
  readability.

**Field-presence rule.** If the request hit one upstream and
succeeded (the common case), `attempted_upstreams[]` has one
entry, `final_upstream` is set, and `fallback_reason` is
omitted. Existing audit-row consumers keep working unchanged
because all three fields are optional additions.

**Persist-vs-compute pin.** All three fields are computed at
write time in the gateway dispatch path. They cannot be
re-derived from request bodies alone — they record runtime
choices. Persist on disk; no on-read recomputation.

**PLAN_QUOTA cross-reference.** A5's
`attempted_upstreams[].rate_limit_reset_at` records the same
reset timestamps PLAN_QUOTA.local.md item M1 captures into
`<gateway>.quota.json`. The audit row is sufficient as a
write-time source for live quota display; PLAN_QUOTA's separate
persistence becomes redundant once A5 lands. Consolidation
opportunity: drop the `<gateway>.quota.json` write and have any
quota endpoint derive its values from the most recent audit row
per gateway.

## 4. Config shape — diff against today

```yaml
# BEFORE
upstream:
  - name: primary
    target: https://api.example.com/v1/chat/completions
    api: openai
    api_key_env: API_KEY

routing:
  - client_model: ["model-*"]
    upstream_name: primary

# AFTER (post-Workstream A)
upstream:
  - name: primary
    target: https://api.example.com/v1/chat/completions
    api: openai
    api_key_envs: [API_KEY_1, API_KEY_2]
    health_probe: false              # opt-in, default false

  - name: anthropic-direct
    target: https://api.anthropic.com
    api: anthropic
    # no api_key_envs — passthrough OAuth from client

routing:
  - client_model: ["model-*"]
    upstream_chain: [primary, anthropic-direct]
```

`health_probe` defaults to false because probes cost real
tokens on translation upstreams. Opt in per upstream.

## 5. Implementation sequence — commits

Each commit lands with its own regression test. No new
dependency added. Each commit aims to stay under 400
production-Go lines per the project's per-commit budget rule.

1. **`feat(gateway): per-upstream health state with observed
   transitions`** — A1. New `health.go` with the state map and
   transition rules. Wired into the response path so 200 / 429
   / 5xx each update state. No probe yet. Admin endpoint
   `GET /api/gateway/upstreams` returns the map.

2. **`feat(gateway): multi-key support per upstream`** — A2.
   Adds `UpstreamCfg.APIKeyEnvs []string` with a backward-
   compatibility shim for `APIKeyEnv string`. Per-key state
   map. `cfg.Validate` rejects empty entries. No rotation
   logic yet — selection picks the first healthy key.

3. **`feat(gateway): rate-limit header parsing and key
   rotation`** — A3. Parsers for `Retry-After`,
   `anthropic-ratelimit-*-reset`, and `x-ratelimit-reset-*`
   plus the single-retry rotation path. Test fixtures for
   each provider.

4. **`feat(gateway): upstream chain in routing rules`** — A4.
   Adds `RoutingRule.UpstreamChain []string` with a
   backward-compatibility shim for `UpstreamName string`.
   Try-in-order fallback driven by per-upstream health.

5. **`feat(gateway): audit row records fallback trail`** — A5.
   `attempted_upstreams[]`, `final_upstream`, and
   `fallback_reason` wired into the audit writer and gateway
   dispatch. Field-presence rules per section 3.A5.

6. **`feat(gateway): optional active probes for unhealthy
   upstreams`** — A1b. Per-gateway probe goroutine, gated on
   `health_probe: true`. Probe traffic tagged
   `X-Mesh-Probe: 1`, excluded from the audit log.

Estimated 4 to 6 commits depending on whether the probe (A1b)
and the admin-UI badge (a sub-component of A1) split. The
admin-UI badge likely lands with A1 if the change is small,
otherwise as a seventh commit alongside the
`/api/gateway/upstreams` view.

## 6. Dependencies

- A3 depends on A2 (multi-key is the prerequisite for
  "rotate to next key").
- A4 depends on A1 (chain selection skips `unhealthy`; without
  A1 there is no health state to skip on).
- A5 depends on A4 (records what was attempted; chain is what
  produces multiple attempts).
- A1b depends on A1 (state machine is what the probe drives).

A1 and A2 are independent; either can land first. The
suggested sequence puts A2 first because the multi-key shape
lands cleanly with no runtime surface change, and A1 wants
observed-traffic test cases that A2 trivially provides.

## 7. Acceptance criteria

Workstream A is accepted when ALL of the following pass.

1. `go build ./...`, `go vet ./...`, `staticcheck ./...`,
   `go test -race ./...` all green.
2. Synthetic two-upstream chain test: primary returns 429,
   fallback returns 200. Audit row carries
   `attempted_upstreams[]` with two entries,
   `final_upstream = fallback`, `fallback_reason =
   "rate_limit"`. Client receives the 200 with no externally
   visible retry.
3. Multi-key test: upstream with two keys; key 0 returns 429
   with `Retry-After: 30`. The same request retries with key 1
   and gets 200. Per-key state shows key 0 degraded with reset
   30 seconds in the future.
4. Connection-failure test: primary upstream's TCP listener is
   shut down mid-test. The first request to it transitions
   `healthy → unhealthy` and falls through to the fallback.
   Subsequent requests skip the unhealthy upstream entirely
   until the probe (or two consecutive successes) flips it
   back.
5. Header-parser unit tests cover the four header families
   listed in section 3.A3, including malformed inputs
   (non-numeric `Retry-After`, RFC 3339 with timezone, RFC 3339
   with offset).
6. Backward-compatibility tests: existing single-key,
   single-upstream configs continue to work without changes.
   `api_key_env` alone resolves to a one-entry `api_key_envs`.
   `upstream_name` alone resolves to a one-entry
   `upstream_chain`.
7. Admin endpoint `GET /api/gateway/upstreams` returns the
   per-upstream and per-key health state. The `last_error`
   field is string-truncated to 256 characters and never
   contains the API key value.
8. Probe-disabled upstream generates zero probe traffic when
   `health_probe: false`. Probe-enabled upstream generates
   one probe per minute while in `unhealthy` state and zero
   probes while healthy.

## 8. Open questions — and defaults

1. **Per-key vs per-upstream health badging in the admin UI.**
   Show keys as sub-rows under the upstream, or roll them up?
   **Default: roll up to the upstream** (one row per upstream;
   show key states only on click). Surface area at single-
   operator scale does not justify two-level badges.
2. **What do we do when every upstream and every key are
   exhausted?** **Default: 429 to the client with
   `Retry-After` set to the soonest reset across all keys. If
   the soonest reset is more than five minutes out, surface a
   503 instead — the operator can re-attempt manually then.**
   Document the threshold in the user-facing message body.
3. **Health state across mesh restarts.** **Default:
   discard.** The state map is process-lifetime. Persisting
   it would couple to the audit-log directory and complicate
   restart semantics for marginal benefit (a 60-second probe
   re-establishes truth).
4. **Should the summarizer call also rotate keys and fall
   through the chain?** **Default: yes, but inherit the
   calling request's chain.** The summarizer is just another
   upstream call from the gateway; the same A2 / A3 / A4 logic
   applies. Worth a call-site test in commit 3 or 4.
5. **Audit row size.** Each `attempted_upstreams[]` entry is
   roughly 200 bytes; chains of three add roughly 600 bytes per
   row in the worst case. The current `resp` row is already 1
   to 4 KB without bodies, so the relative growth is small.
   **Default: no compaction.** Revisit if real traffic pushes
   rows past 16 KB without bodies.

## 9. What this unblocks

- Quota display can read per-upstream and per-key state from
  `/api/gateway/upstreams` and surface "this key resets at
  HH:MM" in any quota status surface.
- PLAN_WORKSTREAM_B's request detail view (item B6, "view raw
  JSONL row") gains the fallback trail to display.
- Phase 1b transformation work (PLAN_WORKSTREAM_C) does NOT
  depend on Workstream A; the two are orthogonal.
