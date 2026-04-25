# Plan — Workstream D: admin UI polish

Status: DRAFT. Date: 2026-04-25. Companion to MESH_VISION,
PLAN_WORKSTREAM_A_resilience, PLAN_WORKSTREAM_B_observability,
PLAN_WORKSTREAM_C_optimization.

## 1. Goal

Land the admin UI ergonomics that Workstream B does not absorb.
Workstream B reshapes the major surfaces — the live session view,
the three view modes, the inspection-action button strip — and
the new surfaces it introduces consume a portion of the existing
PLAN_UX backlog. The remainder is the items B does not touch:
filtering, table layout, column structure, and the small-but-
sharp ergonomics issues an operator hits inside ten minutes of
real use.

Workstream D is independent of Workstream B at the file level
(both edit `cmd/mesh/admin_ui.go` but in non-overlapping regions
for the items below), so the two can run in parallel without a
merge story.

## 2. Scope

### In

- Multi-select filters for gateway and session, with URL-encoded
  filter state (item D1).
- Gateway column in the requests table (item D2).
- Separate input / output token columns in the requests table
  (item D3).
- Table-width control: drop the low-signal Stream column,
  abbreviate headers, and conditionally hide Upstream model when
  it equals client model (item D4).
- Detail card per-pane scrolling so the four-pane grid stays
  navigable when any single pane is large (item D5).
- Overview tab "(all) selected" notice so the blank state is
  self-explanatory (item D6).
- Custom-block TOC sidebar for `<system-reminder>` and similar
  XML blocks that have no markdown headings (item D7).
- One-shot investigation: verify whether upstream-response
  capture works for non-streaming traffic, document the
  streaming limitation in the UI (item D8).

### Out

- The major surface reshapes (live session view, three view
  modes, inspection-action strip, cache-control surfacing).
  Those are Workstream B (items B2, B3, B5, B6) and ship there.
- The timing-bar UI for B1's six-segment partition (Workstream
  B item B1.4).
- Authentication or multi-tenant access control. Loopback admin
  server stays loopback.
- New data on disk. Workstream D works against the row schema
  Phase 1a and Workstream B leave behind; it does not add new
  audit fields.

## 3. Component sketch

### D1 — Multi-select filters with URL-encoded state

Replace the gateway and session single-select dropdowns with
chip-bar multi-select. Empty selection means "all" implicitly —
no artificial `(all)` option.

**Gateway filter.** Replace `<select id="gw-select">` with a
clickable chip bar. Each gateway name is a toggle chip. State
held in `gwSelectedSet: Set<string>`.

**Session filter.** A second chip bar below the gateway bar.
Chip label is `session_id[:8]` plus model plus project path.
State held in `gwSessionSet: Set<string>`. Population from
distinct sessions in the current data window.

**Time period.** Stay as a single dropdown — only one window
makes sense per view.

**URL encoding.** `#requests?gw=foo,bar&sess=abc,def&window=24h&detail=run|id`.
Parse on load (`applyGwHash`), write on change (`writeGwHash`).
The `gw` and `sess` parameters are comma-separated and may be
empty (means "all"). The `detail` parameter retains its current
shape — `run|id` of the open detail card.

This item subsumes PLAN_UX items #2 and #9; their URL-encoding
piece collapses naturally into the same parser.

### D2 — Gateway column in requests table

Add a Gateway column between Time and Session in the requests
table. Source: `p.req.gateway` falling back to `p.resp.gateway`.
Use a stable hash-based color (the existing `sessColor` pattern,
applied to the gateway name) so the same gateway is the same
color across overview, detail card, and table.

This item maps to PLAN_UX item #3.

### D3 — Separate input / output token columns

Replace the single combined `Tokens` column ("in/out" string)
with two columns: `In tokens` and `Out tokens`, each formatted
and color-coded independently.

The color-coding rule from PLAN_UX item #15 is absorbed by
Workstream B item B6 (per `e98ad64`); D3 wires the rule into the
new columns once B6 lands the helper. If D3 ships first, it
applies the existing flat formatting and the color rule lights up
when B6's helper merges. Order-independent.

This item maps to PLAN_UX item #13.

### D4 — Requests table width control

The table grew several columns over recent commits. With Session,
Gateway, Client model, Upstream model, Stream, Status, Outcome,
In tokens, Out tokens, Elapsed, Summary that is 11 columns —
unreadable at common laptop widths.

Three changes:

1. Drop the Stream column. Nearly every request is streaming;
   the column carries no signal.
2. Abbreviate column headers (`Client model` → `Client`,
   `Upstream model` → `Upstream`, `Outcome` → `Result`).
3. Hide the Upstream column by default when it equals Client.
   A toggle reveals it. State held in URL hash so the choice
   persists across reloads.

Do not introduce horizontal scrolling on the table. The
operator-facing UX rule from `principles.md` ("staff-level
craftsmanship on user-facing surfaces") rejects the scroll-
escape; the proper fix is the column drop plus conditional
hide.

This item maps to PLAN_UX observation A.

### D5 — Per-pane scrolling in the detail card

The detail card uses `card-body { max-height: calc(100vh - 180px) }`.
Workstream B item B2.3 introduces a 2×2 four-pane grid; on a
request whose response is large and request is small, the
oversized pane drives the whole card scroll, leaving the small
pane wasted.

Fix: each of the four panes is its own scroll container capped
at `50vh`. The card-level max-height stays as a containment
guarantee, but each pane scrolls independently inside it.

Trade-off: the user loses single-scroll navigation across all
four panes. Mitigation: keyboard shortcuts (`[` and `]` to
focus-next-pane, then arrow keys to scroll) — but only if a
real operator complaint surfaces. v1 is per-pane scroll.

This item maps to PLAN_UX observation B.

### D6 — Overview "(all) selected" notice

When the gateway filter is `(all)` the overview tab today is
blank because the stats-fetch path is skipped. Operators read
this as broken.

Fix: render an explicit notice in the overview body —
"Select a single gateway for overview stats." Style as a muted
info card, not an error. The chip-bar from D1 lets the operator
single-select with one click; the notice should also include a
short hint pointing at the chip bar.

This item maps to PLAN_UX observation C.

### D7 — Custom-block TOC sidebar

`renderCustomBlock` body content goes through `renderPlainText`
→ `renderMdViewer` → `highlightMarkdown`, which already exposes
a TOC for markdown headings. For pure-XML blocks (the
`<system-reminder>` / `<types>` / `<type>` shapes) there are no
headings, so the TOC is empty.

Fix: when a block exceeds 2K characters and has no markdown
headings, parse the top-level XML tags and synthesize a TOC
entry per top-level tag. The XML highlighting helper from
PLAN_UX item #5 (absorbed by Workstream B item B6) is the
natural place to expose the tag-list output; D7 reads from it.

Order: D7 lands after Workstream B's B6 because of the helper
dependency. If a reviewer wants D7 sooner, the parse can live
in the D7 commit and migrate into the B6 helper later.

This item maps to PLAN_UX item #6.

### D8 — Upstream-response data flow check

A short investigation. The `wrapAuditing` middleware populates
`upstream.RespBody` only for non-streaming responses; for
streaming responses, the body is consumed event-by-event and
`upstream.RespBody` stays nil. The `showGwDetail` JS checks
`p.resp.upstream_resp` — for streaming responses this is always
absent.

Two outcomes possible:

1. **Non-streaming traffic does populate the field.** Confirm
   in the audit log (any non-streaming `resp` row carries
   `upstream_resp`). No code change. Add a one-line UI note in
   the upstream-response pane: "Upstream response not available
   for streamed requests."
2. **Non-streaming traffic does not populate the field
   either.** A real bug. Open a separate issue, link from
   here, escalate out of D8 and into A or B depending on where
   the fix lives.

Default expectation is outcome 1 based on a code reading; D8
exists to verify rather than assume.

This item maps to PLAN_UX item #10.

## 4. Sequencing

Items D1–D6 are independent and can ship in any order. D7
depends on Workstream B item B6's XML helper landing first
(unless the parse is duplicated and migrated later). D8 is an
investigation that may resolve into a one-line note rather than
a code change.

A reasonable order, picking smallest-effort first to clear the
backlog:

1. D6 — Overview notice. Single-line addition.
2. D8 — Investigation. May be a one-line UI note.
3. D2 — Gateway column. Small, broadly visible.
4. D3 — Token columns. Mechanical split.
5. D4 — Table width control. Touches column rendering and the
   URL hash.
6. D5 — Per-pane scrolling. Touches the detail card grid.
7. D1 — Multi-select filters with URL state. Largest item;
   touches filter UI, the URL parser, and the chip-bar
   component (which is new).
8. D7 — Custom-block TOC. After Workstream B item B6.

Each lands as its own commit. `FAST=1 task check` after each.

## 5. Source

The substance of this workstream is distilled from
`PLAN_UX.local.md`, the gitignored personal backlog that
informed Workstream B's UI item references. Items absorbed by
Workstream B are listed in `PLAN_WORKSTREAM_B_observability.md`
under the per-item PLAN_UX cross-references; this document
covers the residual.

Items D1–D6 mirror PLAN_UX items 2, 3, 13 and observations A, C
respectively. D5 mirrors observation B. D7 mirrors PLAN_UX item
6. D8 mirrors PLAN_UX item 10.
