# Plan — Workstream B: observability and UI overhaul

Status: COMPLETE for items B1–B6 (commits 1–11 in §5). The
documentation commit (§5 item 12) is the only outstanding piece
and is ambient — the audit-row reference in CLAUDE.md plus this
plan suffice. Date: 2026-04-25. Companion to MESH_VISION,
PLAN_WORKSTREAM_A_resilience, PLAN_WORKSTREAM_C_optimization.

Per-item status (matches the implementation sequence in §5):

| Item | Status | Notes |
|---|---|---|
| B1.1–B1.3 | shipped | Six-segment timing partition; sum-to-total invariant tests pass for streaming and non-streaming. |
| B1.4 | shipped | Stacked horizontal timing bar in request detail card; per-segment colour family (network / mesh / upstream / residual) and hover tooltips. |
| B2.1 | shipped | Session DAG construction backend (hash, parents, branches, in-memory cache). |
| B2.2 | shipped | SSE endpoint at `/api/gateway/sessions/<sid>/events` with reconnection. |
| B2.3 | shipped | Four-pane detail layout. |
| B2.4 | shipped | Frontend linear session view + branch strip + raw toggle. |
| B2.5 | shipped | Graph view with hand-rolled layout + click-to-detail. |
| B2.6 | shipped | Graph interactivity (pan/zoom, hover, edge-diff). |
| B2.7 | shipped | Live-update polish (animation, new-message pill, fork toast). |
| B3.1 | shipped | Anthropic-style chat rendering. |
| B3.2 | shipped | OpenAI-style chat rendering. |
| B3.3 | shipped | Gemini-style chat rendering. (The plan's original "timeline view per session" was redirected: chat-style variants gave more operator value than a separate timeline mode and the timing/attempts info already surfaces in the detail card.) |
| B4.1 | shipped | Active-request registry + admin endpoints. |
| B4.2 | shipped | Frontend chrome indicator + slide-in + per-request live view. |
| B5.1 | shipped | `cache_control` extraction + aggregation backend. |
| B5.2 | shipped | Frontend cache visibility (overview + table + sparkline). |
| B6 | shipped | Inspection actions: view raw JSONL, copy as curl, diff vs another request. |
| §5 #12 | absorbed | Plan + audit-row schema in this document; further docs land per-need. |

## 1. Goal

Turn the audit-row data Phase 1a accumulates into a UI an
operator can actually use. Today the admin UI lists requests in
a table and exposes raw JSONL on click, and not much more.
Diagnosing a live problem ("why did the model just answer the
wrong thing") means tailing files, reading bytes, and translating
SSE by eye.

Workstream B's success criteria:

1. An operator can see end-to-end timing per request decomposed
   into six labelled segments, with the same partition rigor the
   audit-row schema applies to byte counts.
2. An operator can open a session and watch the conversation
   grow live as new turns arrive — no manual refresh, no
   "select-a-newly-coming-request" friction.
3. An operator can choose between three view modes per session:
   raw bytes (four-pane diffable), pretty chat rendering (with
   Anthropic / OpenAI / Gemini visual style toggle), and a
   chronological timeline of events.
4. An operator can see at a glance whether `cache_control`
   markers are working — both per request and as an aggregate
   hit rate.
5. An operator can copy any request as a curl invocation, view
   its raw JSONL row, or diff it against another request without
   leaving the UI.

## 2. Scope

### In

- Six-segment timing partition per request, with a sum-to-total
  invariant (item B1).
- Live session view at `/ui/gateway/sessions/<session_id>`
  (item B2). Polling-based, with an SSE upgrade path documented
  but not shipped in B2.
- Three view modes — Raw, Pretty, Timeline (item B3).
- Live request tail at the top of the admin UI — "N active
  requests" with one-click drill-down (item B4).
- `cache_control` visibility — markers on outgoing request
  blocks plus an aggregated cache hit rate from response usage
  (item B5).
- Per-request inspection actions — copy as curl, view raw
  JSONL, diff against another request (item B6).

### Out

- Editing or replaying requests from the UI. The UI is
  read-only; re-running is a copy-as-curl in a terminal.
- Cost accounting in dollars (covered in MESH_VISION).
- Live-modifying the mesh config from the UI. Config is a YAML
  file the operator edits and `mesh up` re-reads.
- Server-Sent Events push for live session updates in v1.
  Polling at 2 seconds on an open tab is enough at single-
  operator scale; SSE becomes a sub-component of B2 only if
  polling produces measurable load.
- Multi-tenant authentication. Loopback admin server, no auth.

## 3. Component sketch

### B1 — End-to-end timing breakdown

#### B1.1 Timing partition convention

Two invariants govern every timing field. They mirror the
section-byte partition on the byte side; the same rigor applies
because the timing display is just as decision-critical and just
as easy to ship wrong.

**Partition rigor follows SPEC_PHASE1A.local.md §4.1's byte
partition convention** — disjoint sections, exact sum-to-total
invariant, an `other` bucket absorbing residual. The timing
partition (six segments, sum equals total request duration) uses
the same discipline.

**T1. Partition (disjoint).** At any moment in a request's
lifetime, time accumulates into exactly one of the six segments
(or `other`).

**T2. Sum-to-total.** For every request:

```
sum(timing_ms.{client_to_mesh, mesh_translation_in,
              mesh_to_upstream, upstream_processing,
              mesh_translation_out, mesh_to_client, other})
  == total_ms
```

exactly. No tolerance. `other` absorbs scheduler latency,
goroutine wakeup gaps, and short slices the instrumentation
does not cover (for example the dispatch between `wrapAuditing`
and the inner handler). It is the closing residual, not a
free-for-all.

**Counting unit.** Wall-clock milliseconds with sub-millisecond
precision (`time.Duration`) computed via monotonic `time.Now()`
deltas. The audit row stores integer milliseconds to match the
existing `elapsed_ms` precision; sub-millisecond slices fold
into the next-larger segment. Rationale: the operator reads
millisecond-granularity timing; nanosecond noise is not signal.

#### B1.2 Segment definitions

For non-streaming requests:

| Segment | Begins | Ends |
|---|---|---|
| `client_to_mesh` | client TCP accept | gateway handler enters |
| `mesh_translation_in` | gateway handler enters | upstream request bytes ready |
| `mesh_to_upstream` | upstream dial begins | upstream request fully written |
| `upstream_processing` | upstream request fully written | first response byte received |
| `mesh_translation_out` | first response byte received | translated response ready |
| `mesh_to_client` | translated response ready | last byte written to client |

For streaming responses (the default for chat-style clients),
`mesh_translation_out` and `mesh_to_client` interleave.
Redefinitions:

| Segment | Begins / accumulates as |
|---|---|
| `mesh_translation_out` | accumulated wall-clock spent inside the SSE per-event translator across the stream's life |
| `mesh_to_client` | accumulated wall-clock spent inside `Write` to the client across the stream's life |
| `upstream_processing` | accumulated wall-clock spent waiting on the upstream socket's next byte |

Implementation: a small `segmentTimer` struct that the SSE loop
calls `Start(seg)` / `Stop(seg)` on per phase. The three
streaming segments naturally partition the loop's time because
the loop alternates: read upstream → translate → write client →
wait. No phase ever overlaps another; the partition holds by
construction.

#### B1.3 Audit row shape

```json
{
  "t": "resp",
  "id": 84,
  "...": "...",
  "timing_ms": {
    "client_to_mesh": 0,
    "mesh_translation_in": 3,
    "mesh_to_upstream": 12,
    "upstream_processing": 4220,
    "mesh_translation_out": 65,
    "mesh_to_client": 18,
    "other": 2,
    "total": 4320
  }
}
```

`total` placed last, mirroring the section-byte field
convention. Always emitted — no field-presence omission. The
partition is always meaningful; even a 0-byte error response has
`client_to_mesh + mesh_translation_in + other == total`.

For a passthrough gateway (Anthropic-to-Anthropic, OpenAI-to-
OpenAI), `mesh_translation_in` and `mesh_translation_out` are
typically under 1 millisecond each (gzip re-decompression for
the audit-row reassembler is the only real work). The partition
still closes; the values shift toward `upstream_processing` and
`mesh_to_client`.

#### B1.4 UI surface

A stacked horizontal bar in the request detail view, one segment
per color. Hovering a segment shows the millisecond value and a
one-line explanation. Clicking does nothing — the bar is
informational, not interactive in v1. An aggregate version on
the overview tab shows a stacked bar for P50, P95, and P99 per
segment across all requests in the window.

### B2 — Live session view

URL: `/ui/gateway/sessions/<session_id>`. The page state is the
session id; data comes from a paged endpoint that supports
"give me everything since cursor C".

#### B2.1 Backend

```
GET /api/gateway/session/<session_id>?since=<cursor>&limit=<n>

Response:
{
  "session_id": "01HJX...",
  "rows": [
    { "t": "req", "id": 71, "...": "..." },
    { "t": "resp", "id": 71, "...": "..." },
    { "t": "req", "id": 72, "...": "..." }
  ],
  "next_cursor": "01HJX...:72:resp",
  "live": true
}
```

`since` is monotonic over (request id, t-kind). The endpoint
streams whatever is in the audit JSONL files for that session
postdating the cursor, paginated. `live: true` indicates that
further entries may arrive — the SPA polls again with the new
cursor. `live: false` indicates the audit-log file rolled over
and the session id will not appear again (helpful for stopping
the polling loop).

The endpoint reuses the existing audit-stats body walker; no
new parser. Body content is included only at audit-log level
`full`.

#### B2.2 Frontend

The SPA polls every two seconds while the session view is open.
On each poll, append new rows below the existing ones — no
flash, no full reload. Each request renders as a collapsible
row with a one-line summary; clicking expands to the four-pane
raw view (item B2.3) or one of the other view modes (item B3).

The poll cadence stops when the page tab is hidden
(`document.visibilityState === "hidden"`) and resumes on focus.

#### B2.3 Four-pane raw view

Inside an expanded request, a 2×2 grid:

```
┌──────────────────────────┬──────────────────────────┐
│ client → gateway         │ gateway → client         │
│ (request body, headers)  │ (response body, headers) │
├──────────────────────────┼──────────────────────────┤
│ gateway → upstream       │ upstream → gateway       │
│ (translated request)     │ (raw response, decoded)  │
└──────────────────────────┴──────────────────────────┘
```

Each pane is a scrollable code block. The grid headers carry
"Diff" anchors that, when armed against another request in the
same session, render coloured adds and removes. Diff scope:
pane to matching pane only — no cross-pane diffing.

**PLAN_UX cross-reference.** Workstream B2 absorbs
PLAN_UX.local.md item #1 (remove the standalone sessions tab —
the live session view at `/ui/gateway/sessions/<id>` replaces
it) and item #11 (detail layout reorder — the four-pane 2×2
grid in B2.3 is the layout that resolves it).

### B3 — Three view modes per session

A toggle at the top of the session view selects the mode. The
URL hash retains the choice (`#mode=pretty|raw|timeline`).

#### B3.1 Raw

Same as section B2.3. Default mode. Zero enrichment, fast load.

#### B3.2 Pretty

Renders the session as a chat conversation. Each turn shows the
role (system, user, assistant), text content, tool calls
collapsed by default with a `[+] tool_use: Read(...)` expander,
and tool results similarly collapsed.

A second toggle picks the visual style: Anthropic-style (bubble
layout, sky-blue user bubbles, light-grey assistant), OpenAI-
style (linear, monospace tool calls), or Gemini-style (card per
turn). Style is purely cosmetic — same data, different visual
conventions. The operator picks the one that maps to the client
they are debugging.

The pretty view is a faithful render of the conversation as the
gateway sees it post-translation, so an Anthropic-to-OpenAI
request shows the upstream-side OpenAI shape if the operator
toggles to OpenAI style.

#### B3.3 Timeline

Chronological event log:

```
13:18:42.118  request received   id=84  session=222c05da
13:18:42.121  translation_in  3ms      a2o
13:18:42.133  upstream dial   12ms     openai-primary
13:18:42.134  upstream stream begin
13:18:46.354  first content_block_delta (text)
13:18:50.401  message_stop
13:18:50.420  client write done  total 6302 bytes
13:18:50.422  audit row written
```

Events come from the audit row's `timing_ms` (translated to
absolute timestamps), `stream.first_token_ms`, `summarize.fired`,
and the response partition. Each row's timing aligns with the
six-segment partition so the operator can see exactly where the
time went.

Summarization fires render as a dedicated event line:
`summarize: removed 94200 → added 2100 bytes  turns_collapsed=8`.

**Cross-reference.** B3 is partially adjacent to
`archive/PLAN_GATEWAY_SEPARATION.local.md`, which proposed a
`response_model` config field for visually distinguishing
gateway-routed sessions inside the LLM client itself. Both Parts
of that plan have shipped (Part 1: `response_model` field; Part
2: content-breakdown card on the overview tab); Workstream B3
covers only the admin-UI rendering across the three view modes.

### B4 — Live request tail at top of admin UI

A persistent strip above the navigation tabs:

```
[ ⏵ 3 active requests ] gateway-name · 2 streaming · 1 waiting
```

Click to expand a small dropdown listing each active request
with its session id, model, current segment (for example
"upstream_processing 2.1s"), and a click-to-jump shortcut to
the session live view.

"Active" means: the audit row has not been written yet. Backed
by an in-memory map updated as `wrapAuditing` enters and exits
each handler. The map is exposed at `GET /api/gateway/active`
and polled every two seconds.

This replaces today's friction of "open requests tab, sort by
time, click newest".

**PLAN_UX cross-reference.** New surface; no PLAN_UX overlap.

### B5 — `cache_control` visibility

#### Per-request indicator

In the request detail view, a small badge on each block that
carries `cache_control: { type: "ephemeral" }`:

```
[system: 12 KB] [cache: ✓]
[tools: 8 KB]   [cache: ✓]
[tool_results: 108 KB]
```

The badge tells the operator where blocks were marked as
cacheable. Mismatch with what the upstream actually cached
(visible in the response's `usage.cache_read_input_tokens`) is
the primary debugging surface.

#### Aggregated hit rate

In the content breakdown card on the overview tab, a new row:

```
cache hit rate (last 24h): 42% (4.8M of 11.4M cacheable input tokens)
```

Computed as

```
usage.cache_read_input_tokens
  / (usage.cache_read_input_tokens
   + usage.cache_creation_input_tokens
   + usage.input_tokens)
```

summed across requests.

Both fields already exist on Anthropic responses
(`usage.cache_read_input_tokens` and
`usage.cache_creation_input_tokens`). For OpenAI translation
upstreams that do not emit them, the row shows "—" with a
tooltip explaining why.

Implementation: extend the audit row's `usage` field to carry
both cache-token counts (already present on `MessagesResponse`,
just plumb through). The aggregate is computed at stats-fetch
time in the existing audit-stats handler.

**PLAN_UX cross-reference.** New surface; no PLAN_UX overlap.

### B6 — Per-request inspection actions

Three buttons on the request detail header:

#### "Copy as curl"

Emits a curl invocation reproducing the upstream request:

```sh
curl -sS https://api.example.com/v1/chat/completions \
  -H 'authorization: Bearer $API_KEY' \
  -H 'content-type: application/json' \
  -x http://127.0.0.1:1081 \
  -d @- <<'JSON'
{ "model": "...", "...": "..." }
JSON
```

The Authorization header value is replaced with the
environment-variable name literally (not the actual key) so
the operator can paste safely. Other headers are passed through
verbatim.

#### "View raw JSONL row"

Opens a modal showing the audit row exactly as written to disk.
This is the same JSONL `jq` would print. Useful when the
operator suspects the UI's enrichment is wrong and wants
ground truth.

#### "Diff against another request"

Two-step: click the button, then click another request in the
same session. The detail view splits into two columns, each
pane diffed against its sibling. Diff scope is per-section
(system vs system, tools vs tools) using a structural diff —
not a raw JSON line diff — so reordered messages of equal
content show as no-change.

The structural differ uses the existing section-byte boundaries
as the section definition, then runs unified-diff inside each
section.

**PLAN_UX cross-reference.** Workstream B6 absorbs
PLAN_UX.local.md item #4 (markdown TOC scroll fix), item #5
(XML syntax highlighting in the markdown viewer), item #7 (TOC
char-count badge styling), item #8 (collapsible client/upstream
request/response panes), item #12 (consistent model colors
across the UI), item #14 (human-readable elapsed time and
visual cues), and item #15 (token-count formatting and color
coding). All seven are inspection-action ergonomics that the B6
button strip and detail card naturally subsume.

**PLAN_UX items not absorbed by Workstream B.** Item #2
(multi-select filters), item #3 (gateway column in the requests
table), item #9 (URL-encoded filter state), item #13 (separate
input/output token columns), item #6 (custom-block TOC sidebar),
item #10 (upstream-response data-flow check), and the author's
observations A, B, and C are tracked separately in
`PLAN_WORKSTREAM_D_uxpolish.md`. Workstream D runs in parallel
with Workstream B without conflict.

## 4. Audit row additions

This workstream adds two field families to the existing `resp`
row:

```json
{
  "t": "resp",
  "id": 84,
  "...": "...",
  "timing_ms": { "...": "B1 — six segments + other + total" },
  "usage": {
    "input_tokens": 12000,
    "output_tokens": 410,
    "cache_creation_input_tokens": 8400,
    "cache_read_input_tokens": 0
  }
}
```

`usage.cache_*` fields already exist on Anthropic
`MessagesResponse`; the gateway captures them and plumbs them
through (item B5). `timing_ms` is a new top-level field
(item B1). No breaking changes to existing fields.

Persist-vs-compute pin:

| Field | On disk? | Computed where |
|---|---|---|
| `timing_ms.*` | Yes | Write-time. Each segment's end call records its delta into the audit row. |
| `usage.cache_*` | Yes | Write-time. Plumbed from the upstream `usage` object. |
| `cache_control` markers in detail UI | No | Read-time: parse request body via the existing section-byte walker, surface block-level marker booleans. |
| Aggregate cache hit rate | No | Read-time at `/api/gateway/audit/stats` from per-row `usage.cache_*`. |

## 5. Implementation sequence — commits

Each commit lands its own regression test. The estimate is 8 to
12 commits depending on splits.

1. **`feat(gateway): six-segment timing partition with sum-to-total invariant`**
   — items B1.1, B1.2, B1.3 (backend half). `segmentTimer`
   struct; instrumentation in non-streaming and streaming
   dispatch; audit row carries `timing_ms`. Partition tests
   assert T1 and T2 hold for both modes.
2. **`feat(admin): stacked timing breakdown bar in request detail view`**
   — item B1.4 (frontend half). Bar component, per-segment
   colours, hover tooltip, aggregate version on the overview tab.
3. **`feat(admin): live session view backend`** — item B2.1.
   `/api/gateway/session/<id>` endpoint with cursor pagination,
   `live: true|false` flag, session-id resolution per the
   existing header-based session resolution chain.
4. **`feat(admin): live session view SPA component`** — items
   B2.2 and B2.3. Polling SPA component, four-pane expansion,
   diff anchor armed-state.
5. **`feat(admin): pretty conversation view with style toggle`**
   — item B3.2. Renderer for chat-style display, three visual
   style presets, tool-call collapse-by-default.
6. **`feat(admin): timeline view per session`** — item B3.3.
   Event log derived from audit-row fields; alignment with the
   B1 segments.
7. **`feat(gateway): live request tail backend`** — item B4
   (backend). In-memory active-request map,
   `/api/gateway/active` endpoint.
8. **`feat(admin): live request tail strip in nav`** — item B4
   (frontend). Persistent strip above nav, click-to-drill-down.
9. **`feat(gateway): cache_control marker detection and usage.cache_* plumb-through`**
   — item B5 (backend). Plumb both cache-token fields through
   the audit writer; the section-byte walker surfaces block-
   level cache markers.
10. **`feat(admin): cache_control visibility in detail and overview`**
    — item B5 (frontend). Per-block badge plus aggregate
    hit-rate row in the breakdown card.
11. **`feat(admin): copy-as-curl, view raw JSONL, diff actions`**
    — item B6. Three buttons on the detail header. The diff uses
    the structural differ keyed by section-byte boundaries.
12. **`docs(gateway): document Workstream B audit fields and UI surfaces`**
    — gateway documentation update plus a short companion
    section in `docs/gateway/` if needed. Mark fields stable.

Splits are likely if any commit breaks the production-Go line
budget:

- Commit 1 may split into "non-streaming" plus "streaming" if
  the segment-timer instrumentation in the SSE loop bloats. The
  partition test stays in commit 1; instrumentation lands
  incrementally.
- Commit 4 may split into "raw view + diff" plus "polling
  scaffolding" if the SPA component grows.

## 6. Acceptance criteria

Workstream B is accepted when ALL of the following pass.

1. `go build ./...`, `go vet ./...`, `staticcheck ./...`,
   `go test -race ./...` all green.
2. **Timing partition closure (T2).** For 100 captured
   requests replayed through the gateway,
   `sum(timing_ms.*) == total_ms` exactly for every row, in
   both streaming and non-streaming modes.
3. **Timing partition disjointness (T1).** Unit test on a
   synthetic dispatch where each segment is forced to a known
   duration via `time.Sleep` instrumentation hooks; assert
   exactly one segment increments per simulated phase.
4. **Live session view freshness.** Write 10 fake `req+resp`
   row pairs to a JSONL file at one-per-second cadence. Open
   the session view; assert the SPA receives all 10 within
   three seconds of the last write, with no manual refresh.
5. **Active request map.** Start a long upstream call (3
   seconds sleep). `GET /api/gateway/active` returns one entry
   while the call is in flight; the entry disappears within
   100 milliseconds of the response being written.
6. **`cache_control` surfacing.** Anthropic request with
   `cache_control: ephemeral` on the system block: detail view
   shows the cache badge on system. Aggregate row shows
   `cache_read_input_tokens > 0` after a second identical
   request lands.
7. **Diff correctness.** Two requests that differ only by
   their most-recent user turn show only that turn as different
   in the structural diff. Section reordering (system before
   tools vs tools before system) does NOT show as a difference.
8. **Copy-as-curl reproducibility.** Manually executing the
   emitted curl (after substituting the env var) produces a
   200 from the upstream. Verified once per provider during
   commit 11 review.
9. **Backward compat.** Existing audit rows (pre-B) still load
   in the requests table with timing fields absent. The UI
   degrades gracefully: the bar shows "timing unavailable for
   pre-B rows" once.
10. **No new third-party dependencies.** All UI work uses the
    existing Svelte stack and stdlib `encoding/json`. No diff-
    library dependency — the structural differ is a thin
    wrapper over section-byte boundaries.

## 7. Open questions — and defaults

1. **Polling cadence.** Two seconds feels right but is
   unmeasured. **Default: 2 seconds.** If profiling shows admin
   process load above 5% on a long-running session view, drop
   to 5 seconds with a "click to refresh" override. Move to
   SSE only if 5 seconds is too slow.
2. **Pretty mode style toggle persistence.** Per-tab or per-
   user? **Default: per-tab via URL hash.** No `localStorage`
   — keep state navigable via URL.
3. **Timeline granularity.** Per-event (every SSE delta
   visible) or aggregated? **Default: aggregated.** SSE delta-
   level timeline is overwhelming; show summarize, first-token,
   `message_stop`, and errors only. v2 may add a "verbose"
   toggle.
4. **Diff scope across sessions.** Allow diffing requests from
   different sessions? **Default: no in v1.** Same-session
   diff covers the "what changed between turns" question;
   cross-session diff is rare and would complicate the UI.
5. **`cache_control` aggregate denominator.** Include
   `output_tokens` in the denominator (no — output is not
   cacheable input) or not? **Default: no.** Hit-rate
   denominator is `cache_read + cache_creation + input_tokens`;
   output is excluded. Document the formula in B5's tooltip.
6. **Live tail strip click behavior.** Open in current tab or
   a new one? **Default: current tab; push history state.**
   New-tab is a right-click affordance browsers handle
   natively.

## 8. Risk and notes

- **Audit-log level `metadata` degrades B2 and B3 sharply.**
  Without bodies on disk, the four-pane view shows headers only
  and the pretty view is empty. The UI must render an "Enable
  `log: full` for body view" hint, not a blank page.
- **Long-session memory.** A session of 500 turns with full
  bodies could be 50 MB. The live view should virtualize the
  scroll list (render only visible rows) — Svelte's
  `<svelte:component>` with intersection observer is the
  pattern. Land in commit 4.
- **Stream-end races.** Live view polling could see a request
  appear in `/api/gateway/active` and then fail to find it in
  `/api/gateway/session/<id>` for around 100 milliseconds (audit
  row not yet flushed). Treat as eventually-consistent; the UI
  shows "writing..." for in-flight rows.

## 9. What this unblocks

- PLAN_WORKSTREAM_C (Phase 1b transformation) becomes
  evidence-driven once B's surfaces let the operator see
  exactly which sessions blow up the context. Without B, C is
  forecasting from JSONL files.
- A future "share this session" affordance (export the
  conversation as a redacted bundle) becomes straightforward
  once B2 and B3 exist; not in scope for v1.
- The metrics-and-quota axis gains a UI surface to mirror; the
  rate-limit display can live in the same nav strip as item B4.
