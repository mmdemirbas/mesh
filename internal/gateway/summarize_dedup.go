package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// summarizerCacheTTL is how long a completed summarizer result stays
// warm before re-firing. Concurrent callers arriving during a flight
// share the channel result via singleflight regardless of this TTL.
const summarizerCacheTTL = 60 * time.Second

// summarizerMaxCallDuration bounds a single summarizer invocation. On
// timeout the inner context cancels; the error propagates to all
// joined callers.
const summarizerMaxCallDuration = 30 * time.Second

// summarizerCacheCap caps the number of cached summaries per gateway.
// Summarization fires rarely and each entry is a small []AnthropicMsg;
// 128 is far above the steady-state working set for a single user and
// the ad-hoc sweep keeps the map bounded without a background ticker.
const summarizerCacheCap = 128

// cachedSummary is a completed summarizer result with the timestamp
// used for TTL and sweep decisions. The stored value is the summary
// text — the outer call stitches it into an AnthropicMsg and appends
// the caller's recent tail, so the cache payload stays small and
// independent of per-caller context.
type cachedSummary struct {
	text string
	ts   time.Time
}

// summarizerDedup collapses concurrent summarizer calls with the same
// (upstream, prefix) input into a single upstream request and caches
// the result for summarizerCacheTTL so a caller arriving within that
// window after completion also skips the upstream call.
//
// One dedup instance per Router (per gateway). Two gateways with
// different summarizers never share cache entries; neither do two
// requests to the same summarizer with different prefix content.
//
// Semantics pin:
//   - Key = sha256(upstream_name || 0x00 || json.Marshal(messages[:cutoff])).
//     session_id is deliberately excluded: the summarizer's output is a
//     pure function of its input messages, so two sessions with
//     identical prefixes benefit from sharing the result. Add session_id
//     back only when summarization becomes session-specific.
//   - Wait-and-share: concurrent callers with the same key block on a
//     single singleflight flight; they all receive the same result
//     channel. No caller bypasses summarization — that would defeat
//     the point (an oversized request would sail upstream).
//   - Shared-cancel: the flight's inner context derives from the
//     leader's context with a summarizerMaxCallDuration cap. If the
//     leader cancels (client disconnected, request timed out), the
//     flight aborts and joined callers see the cancel error. A
//     subsequent caller will re-fire with their own context.
//   - Completion-cached: on success, the result is stored in the cache
//     inside the singleflight fn — so the cache is populated even if
//     the leader walked away but the flight succeeded. Singleflight's
//     own internal map auto-cleans on fn return; our cache keeps
//     serving for the TTL window.
//
// TODO (multi-caller HTTP reality): the shared-cancel semantic above
// punishes joined callers for the leader's disconnect. In a production
// gateway where each caller has its own live HTTP request, caller A
// hanging up would abort the flight and surface a cancel error to
// caller B, who is still connected and still wanting an answer. The
// intended behavior is to detach the flight's ctx from the leader —
// flight runs under a gateway-owned ctx with only the 30s max-call
// timeout, joined callers ride to completion regardless of who
// connects or disconnects. Revisit when we have Phase 1a data on
// actual fan-out patterns and real-world cancel rates.
type summarizerDedup struct {
	sf singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedSummary
	clock func() time.Time
}

func newSummarizerDedup() *summarizerDedup {
	return &summarizerDedup{
		cache: make(map[string]cachedSummary),
		clock: time.Now,
	}
}

// dedupKey returns the stable hash key for a (upstream, prefix) pair.
// json.Marshal on []AnthropicMsg is deterministic at this depth: slice
// order is preserved, AnthropicMsg has no map fields, and RawMessage
// contents are the bytes the client sent verbatim.
func dedupKey(upstreamName string, prefixJSON []byte) string {
	h := sha256.New()
	h.Write([]byte(upstreamName))
	h.Write([]byte{0})
	h.Write(prefixJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// do runs fn under single-flight dedup. Semantics: see the summarizerDedup
// doc comment for the pin.
func (d *summarizerDedup) do(
	ctx context.Context,
	key string,
	fn func(context.Context) (string, error),
) (string, error) {
	// Cache-hit fast path. Miss or stale entry falls through to the
	// singleflight call below.
	d.mu.Lock()
	if entry, ok := d.cache[key]; ok {
		if d.clock().Sub(entry.ts) < summarizerCacheTTL {
			d.mu.Unlock()
			return entry.text, nil
		}
		// Stale — drop now so the map doesn't grow while the flight runs.
		delete(d.cache, key)
	}
	d.mu.Unlock()

	ch := d.sf.DoChan(key, func() (any, error) {
		innerCtx, cancel := context.WithTimeout(ctx, summarizerMaxCallDuration)
		defer cancel()
		text, err := fn(innerCtx)
		if err == nil {
			d.store(key, text)
		}
		return text, err
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

// store caches a summary result, evicting stale entries first if the
// map is at capacity. Keeping the sweep inline with the write keeps
// the cache bounded without a background goroutine.
func (d *summarizerDedup) store(key, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cache) >= summarizerCacheCap {
		d.sweepStaleLocked()
		// If sweep didn't free a slot (no stale entries), drop the
		// oldest by timestamp — simple bound, no LRU bookkeeping.
		if len(d.cache) >= summarizerCacheCap {
			var oldestKey string
			var oldestTS time.Time
			for k, e := range d.cache {
				if oldestKey == "" || e.ts.Before(oldestTS) {
					oldestKey = k
					oldestTS = e.ts
				}
			}
			delete(d.cache, oldestKey)
		}
	}
	d.cache[key] = cachedSummary{text: text, ts: d.clock()}
}

func (d *summarizerDedup) sweepStaleLocked() {
	cutoff := d.clock().Add(-summarizerCacheTTL)
	for k, e := range d.cache {
		if e.ts.Before(cutoff) {
			delete(d.cache, k)
		}
	}
}
