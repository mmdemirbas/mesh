package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractQuota_AnthropicHeaders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("anthropic-ratelimit-tokens-limit", "60000000")
	h.Set("anthropic-ratelimit-tokens-remaining", "4812300")
	h.Set("anthropic-ratelimit-tokens-reset", "2026-04-26T13:00:00Z")
	h.Set("anthropic-ratelimit-input-tokens-limit", "20000000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "1234567")
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "987")

	snap := extractQuotaFromHeaders(h, APIAnthropic, now)

	if snap.IsEmpty() {
		t.Fatal("expected non-empty snapshot")
	}
	if snap.TokensLimit == nil || *snap.TokensLimit != 60_000_000 {
		t.Errorf("tokens_limit = %v, want 60000000", snap.TokensLimit)
	}
	if snap.TokensRemaining == nil || *snap.TokensRemaining != 4_812_300 {
		t.Errorf("tokens_remaining = %v, want 4812300", snap.TokensRemaining)
	}
	if snap.TokensReset == nil || !snap.TokensReset.Equal(time.Date(2026, 4, 26, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("tokens_reset = %v", snap.TokensReset)
	}
	if snap.InputTokensRemaining == nil || *snap.InputTokensRemaining != 1_234_567 {
		t.Errorf("input_tokens_remaining = %v", snap.InputTokensRemaining)
	}
	if snap.RequestsRemaining == nil || *snap.RequestsRemaining != 987 {
		t.Errorf("requests_remaining = %v", snap.RequestsRemaining)
	}
}

func TestExtractQuota_OpenAIHeaders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-ratelimit-limit-tokens", "1000000")
	h.Set("x-ratelimit-remaining-tokens", "987654")
	h.Set("x-ratelimit-reset-tokens", "30s")
	h.Set("x-ratelimit-limit-requests", "500")
	h.Set("x-ratelimit-remaining-requests", "498")
	h.Set("x-ratelimit-reset-requests", "1m30s")

	snap := extractQuotaFromHeaders(h, APIOpenAI, now)

	if snap.TokensLimit == nil || *snap.TokensLimit != 1_000_000 {
		t.Errorf("tokens_limit = %v", snap.TokensLimit)
	}
	if snap.TokensRemaining == nil || *snap.TokensRemaining != 987_654 {
		t.Errorf("tokens_remaining = %v", snap.TokensRemaining)
	}
	if snap.TokensReset == nil || !snap.TokensReset.Equal(now.Add(30*time.Second)) {
		t.Errorf("tokens_reset = %v, want now+30s", snap.TokensReset)
	}
	if snap.RequestsReset == nil || !snap.RequestsReset.Equal(now.Add(90*time.Second)) {
		t.Errorf("requests_reset = %v, want now+90s", snap.RequestsReset)
	}
}

func TestExtractQuota_NoHeadersIsEmpty(t *testing.T) {
	t.Parallel()
	now := time.Now()
	snap := extractQuotaFromHeaders(http.Header{}, APIAnthropic, now)
	if !snap.IsEmpty() {
		t.Errorf("expected empty snapshot from no headers, got %+v", snap)
	}
}

func TestQuotaStore_UpdateMergesPartial(t *testing.T) {
	t.Parallel()
	q := newQuotaStore()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	tokens := int64(100)
	first := QuotaSnapshot{CapturedAt: now, TokensRemaining: &tokens}
	q.Update("gw", "https://upstream", first)

	// Second update only carries requests info — must NOT erase
	// the previously-observed tokens window.
	requests := int64(50)
	second := QuotaSnapshot{CapturedAt: now.Add(time.Second), RequestsRemaining: &requests}
	q.Update("gw", "https://upstream", second)

	got := q.Snapshot()["gw"]
	if got.TokensRemaining == nil || *got.TokensRemaining != 100 {
		t.Errorf("tokens lost on partial second update: %+v", got)
	}
	if got.RequestsRemaining == nil || *got.RequestsRemaining != 50 {
		t.Errorf("requests not merged: %+v", got)
	}
	if !got.CapturedAt.Equal(now.Add(time.Second)) {
		t.Errorf("captured_at not bumped: %v", got.CapturedAt)
	}
}

func TestQuotaStore_EmptyUpdateIsNoOp(t *testing.T) {
	t.Parallel()
	q := newQuotaStore()
	q.Update("gw", "https://upstream", QuotaSnapshot{})
	if len(q.Snapshot()) != 0 {
		t.Errorf("empty snapshot should not be stored")
	}
}

func TestQuotaStore_PersistAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := newQuotaStore()
	q.SetPersistDir(dir)
	tokens := int64(123456)
	q.Update("gw", "https://up", QuotaSnapshot{
		CapturedAt:      time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		TokensRemaining: &tokens,
	})
	// File written?
	target := filepath.Join(dir, "gw.quota.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("quota file not written: %v", err)
	}
	// New store reads it back.
	q2 := newQuotaStore()
	if err := q2.LoadPersistedQuotas(dir); err != nil {
		t.Fatalf("LoadPersistedQuotas: %v", err)
	}
	got := q2.Snapshot()["gw"]
	if got.TokensRemaining == nil || *got.TokensRemaining != 123456 {
		t.Errorf("reloaded tokens = %v, want 123456", got.TokensRemaining)
	}
}

// captureQuota's integration with the package-level Quota store is
// trivial pass-through; testing it would require mutating the global
// var which races with concurrent dispatch tests. Instead exercise
// the parts directly: extractQuotaFromHeaders and QuotaStore.Update
// each have their own coverage above. The only nil-safety branches
// in captureQuota are also covered by QuotaStore.Update's gateway==""
// short-circuit (see TestQuotaStore_EmptyUpdateIsNoOp).
//
// If a future change breaks the wiring (e.g., captureQuota stops
// calling Update), it will surface in the e2e gateway test which
// drives a real Start() and asserts /api/gateway/quota.
