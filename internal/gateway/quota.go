package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PLAN_QUOTA: upstream-authoritative quota cache.
//
// Every successful upstream response carries the upstream provider's
// current view of the rate-limit window in standardized headers
// (Anthropic anthropic-ratelimit-* RFC 3339, OpenAI x-ratelimit-*
// duration strings). mesh extracts these on every audited response,
// keeps the latest per gateway in memory, and exposes them via the
// admin API so external consumers (claude-quota statusline plugin)
// can show real upstream-authoritative quota state without
// duplicating the parsers.
//
// This file implements M1 (header capture, normalization, in-memory
// store, on-disk persistence) and the snapshot accessor. M2 (the
// admin endpoint) wires from cmd/mesh.

// QuotaSnapshot is the normalized per-gateway rate-limit state. All
// numeric fields are pointers so "header absent / not yet observed"
// stays distinguishable from "header present and zero" — claude-quota
// renders the absent case as "—" instead of "0 remaining".
type QuotaSnapshot struct {
	CapturedAt time.Time `json:"captured_at"`
	// UpstreamURL is the target the gateway is dispatching to —
	// useful for the consumer to label which provider this snapshot
	// came from (panshi, anthropic, openai, ...).
	UpstreamURL string `json:"upstream_url,omitempty"`

	TokensRemaining *int64     `json:"tokens_remaining,omitempty"`
	TokensLimit     *int64     `json:"tokens_limit,omitempty"`
	TokensReset     *time.Time `json:"tokens_reset,omitempty"`

	RequestsRemaining *int64     `json:"requests_remaining,omitempty"`
	RequestsLimit     *int64     `json:"requests_limit,omitempty"`
	RequestsReset     *time.Time `json:"requests_reset,omitempty"`

	// Anthropic emits separate input/output token windows.
	InputTokensRemaining *int64     `json:"input_tokens_remaining,omitempty"`
	InputTokensLimit     *int64     `json:"input_tokens_limit,omitempty"`
	InputTokensReset     *time.Time `json:"input_tokens_reset,omitempty"`

	OutputTokensRemaining *int64     `json:"output_tokens_remaining,omitempty"`
	OutputTokensLimit     *int64     `json:"output_tokens_limit,omitempty"`
	OutputTokensReset     *time.Time `json:"output_tokens_reset,omitempty"`
}

// IsEmpty reports whether the snapshot has no observed numeric
// fields — used to decide whether the upstream emitted any
// rate-limit headers at all.
func (s QuotaSnapshot) IsEmpty() bool {
	return s.TokensRemaining == nil && s.TokensLimit == nil &&
		s.RequestsRemaining == nil && s.RequestsLimit == nil &&
		s.InputTokensRemaining == nil && s.InputTokensLimit == nil &&
		s.OutputTokensRemaining == nil && s.OutputTokensLimit == nil
}

// extractQuotaFromHeaders parses the Anthropic and OpenAI rate-limit
// header families into a normalized snapshot. api selects which
// family to walk (Anthropic uses RFC 3339 reset times; OpenAI uses
// duration strings like "1m23s" or absolute UNIX seconds depending
// on the provider). Unknown / missing headers leave fields nil.
func extractQuotaFromHeaders(h http.Header, api string, now time.Time) QuotaSnapshot {
	snap := QuotaSnapshot{CapturedAt: now}
	switch api {
	case APIAnthropic:
		extractAnthropicQuota(h, now, &snap)
	case APIOpenAI:
		extractOpenAIQuota(h, now, &snap)
	default:
		// Try both as a last resort — some upstream proxies forward
		// either family regardless of the underlying API shape.
		extractAnthropicQuota(h, now, &snap)
		extractOpenAIQuota(h, now, &snap)
	}
	return snap
}

func extractAnthropicQuota(h http.Header, now time.Time, out *QuotaSnapshot) {
	// anthropic-ratelimit-requests-{limit,remaining,reset}
	if v := h.Get("anthropic-ratelimit-requests-limit"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.RequestsLimit = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-requests-remaining"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.RequestsRemaining = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-requests-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.RequestsReset = &t
		}
	}
	// anthropic-ratelimit-tokens-{limit,remaining,reset} — combined window.
	if v := h.Get("anthropic-ratelimit-tokens-limit"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.TokensLimit = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-tokens-remaining"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.TokensRemaining = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-tokens-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.TokensReset = &t
		}
	}
	// anthropic-ratelimit-input-tokens-{limit,remaining,reset}
	if v := h.Get("anthropic-ratelimit-input-tokens-limit"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.InputTokensLimit = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-remaining"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.InputTokensRemaining = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.InputTokensReset = &t
		}
	}
	// anthropic-ratelimit-output-tokens-{limit,remaining,reset}
	if v := h.Get("anthropic-ratelimit-output-tokens-limit"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.OutputTokensLimit = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-output-tokens-remaining"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.OutputTokensRemaining = &n
		}
	}
	if v := h.Get("anthropic-ratelimit-output-tokens-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.OutputTokensReset = &t
		}
	}
}

func extractOpenAIQuota(h http.Header, now time.Time, out *QuotaSnapshot) {
	// x-ratelimit-{limit,remaining}-{requests,tokens}
	// x-ratelimit-reset-{requests,tokens} — duration strings.
	if v := h.Get("x-ratelimit-limit-requests"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.RequestsLimit = &n
		}
	}
	if v := h.Get("x-ratelimit-remaining-requests"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.RequestsRemaining = &n
		}
	}
	if v := h.Get("x-ratelimit-reset-requests"); v != "" {
		if t, ok := parseOpenAIReset(v, now); ok {
			out.RequestsReset = &t
		}
	}
	if v := h.Get("x-ratelimit-limit-tokens"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.TokensLimit = &n
		}
	}
	if v := h.Get("x-ratelimit-remaining-tokens"); v != "" {
		if n, ok := parseInt64(v); ok {
			out.TokensRemaining = &n
		}
	}
	if v := h.Get("x-ratelimit-reset-tokens"); v != "" {
		if t, ok := parseOpenAIReset(v, now); ok {
			out.TokensReset = &t
		}
	}
}

// parseOpenAIReset accepts either a Go-style duration ("1m23s",
// "500ms") that resolves to "now + d", or a UNIX timestamp in
// seconds. Returns the absolute reset time.
func parseOpenAIReset(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(d), true
	}
	if n, ok := parseInt64(v); ok && n > 0 {
		// Heuristic: < 1e10 means seconds-since-epoch (post-2001),
		// not "seconds from now". OpenAI hands absolute timestamps.
		if n > 1_000_000_000 {
			return time.Unix(n, 0), true
		}
		return now.Add(time.Duration(n) * time.Second), true
	}
	return time.Time{}, false
}

func parseInt64(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// QuotaStore caches the latest QuotaSnapshot per gateway name.
// Process-wide singleton accessed via the package-level Quota var;
// tests build their own via newQuotaStore.
type QuotaStore struct {
	mu         sync.RWMutex
	snapshots  map[string]QuotaSnapshot
	persistDir string
}

// Quota is the process-wide quota store. Populated by gateway
// dispatch on every successful upstream response; read by the admin
// endpoint.
var Quota = newQuotaStore()

func newQuotaStore() *QuotaStore {
	return &QuotaStore{snapshots: map[string]QuotaSnapshot{}}
}

// SetPersistDir configures the directory where quota snapshots are
// persisted to disk on every Update. Pass empty to disable
// persistence (default; tests).
func (q *QuotaStore) SetPersistDir(dir string) {
	q.mu.Lock()
	q.persistDir = dir
	q.mu.Unlock()
}

// Update merges a new snapshot for the named gateway. Empty
// snapshots (no observed fields) are skipped — the upstream emitted
// no rate-limit headers, so there's nothing to record. Already-known
// fields are overwritten with the new values; fields nil in the new
// snapshot keep their last-seen value (so a partial response —
// requests headers but no tokens headers — does not erase the
// previously-captured tokens window).
func (q *QuotaStore) Update(gateway, upstreamURL string, snap QuotaSnapshot) {
	if q == nil || gateway == "" || snap.IsEmpty() {
		return
	}
	snap.UpstreamURL = upstreamURL
	q.mu.Lock()
	prev := q.snapshots[gateway]
	merged := mergeSnapshots(prev, snap)
	q.snapshots[gateway] = merged
	persistDir := q.persistDir
	q.mu.Unlock()
	if persistDir != "" {
		_ = persistQuota(persistDir, gateway, merged)
	}
}

// mergeSnapshots takes the last-seen merged with new. The new
// snapshot's CapturedAt and UpstreamURL win; numeric/time fields
// from new replace prev's when non-nil, otherwise prev's value
// carries forward.
func mergeSnapshots(prev, next QuotaSnapshot) QuotaSnapshot {
	out := next
	if out.TokensRemaining == nil {
		out.TokensRemaining = prev.TokensRemaining
	}
	if out.TokensLimit == nil {
		out.TokensLimit = prev.TokensLimit
	}
	if out.TokensReset == nil {
		out.TokensReset = prev.TokensReset
	}
	if out.RequestsRemaining == nil {
		out.RequestsRemaining = prev.RequestsRemaining
	}
	if out.RequestsLimit == nil {
		out.RequestsLimit = prev.RequestsLimit
	}
	if out.RequestsReset == nil {
		out.RequestsReset = prev.RequestsReset
	}
	if out.InputTokensRemaining == nil {
		out.InputTokensRemaining = prev.InputTokensRemaining
	}
	if out.InputTokensLimit == nil {
		out.InputTokensLimit = prev.InputTokensLimit
	}
	if out.InputTokensReset == nil {
		out.InputTokensReset = prev.InputTokensReset
	}
	if out.OutputTokensRemaining == nil {
		out.OutputTokensRemaining = prev.OutputTokensRemaining
	}
	if out.OutputTokensLimit == nil {
		out.OutputTokensLimit = prev.OutputTokensLimit
	}
	if out.OutputTokensReset == nil {
		out.OutputTokensReset = prev.OutputTokensReset
	}
	return out
}

// Snapshot returns a deep-enough copy of every gateway's latest
// quota state. The returned map is owned by the caller; mutating it
// has no effect on the store. Call from /api/gateway/quota.
func (q *QuotaStore) Snapshot() map[string]QuotaSnapshot {
	if q == nil {
		return nil
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make(map[string]QuotaSnapshot, len(q.snapshots))
	for k, v := range q.snapshots {
		out[k] = v
	}
	return out
}

// persistQuota atomically writes the snapshot to
// <dir>/<gateway>.quota.json. Best-effort; errors are swallowed so
// a bad permissions / disk-full situation does not break the
// gateway dispatch path.
func persistQuota(dir, gateway string, snap QuotaSnapshot) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dir, gateway+".quota.json")
	tmp := target + ".tmp"
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// captureQuota is the dispatch-side hook called from every successful
// upstream response site. Pulls the rate-limit headers off the
// response, normalizes per the upstream's API shape, and merges the
// snapshot into the process-wide store. Cheap on the no-headers
// path: extractQuotaFromHeaders returns an empty snapshot, Update
// short-circuits on IsEmpty().
func captureQuota(gwName string, upstream *ResolvedUpstream, h http.Header, now time.Time) {
	if gwName == "" || upstream == nil || h == nil {
		return
	}
	snap := extractQuotaFromHeaders(h, upstream.Cfg.API, now)
	Quota.Update(gwName, upstream.Cfg.Target, snap)
}

// LoadPersistedQuotas reads any *.quota.json files in dir and seeds
// the store. Called once at gateway startup so the admin endpoint
// returns last-known state immediately, without waiting for the
// first upstream response after restart.
func (q *QuotaStore) LoadPersistedQuotas(dir string) error {
	if q == nil || dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".quota.json") {
			continue
		}
		gateway := strings.TrimSuffix(e.Name(), ".quota.json")
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var snap QuotaSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			continue
		}
		q.snapshots[gateway] = snap
	}
	return nil
}
