package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Workstream A.4 — rate-limit signal parsing.
//
// When an upstream returns 429 (or 529 for Anthropic overload), mesh
// extracts the operator's retry hint from the response and uses it to
// degrade the offending key for the right window. Three sources, in
// priority order:
//
//   1. Retry-After header (RFC 7231) — accepted everywhere; in
//      seconds (integer) or HTTP-date format. Authoritative when
//      present.
//   2. Anthropic-specific reset header
//      `anthropic-ratelimit-{requests,tokens}-reset` — RFC 3339
//      timestamps. Used when Retry-After is missing.
//   3. OpenAI-specific reset header `x-ratelimit-reset-{tokens,
//      requests}` — duration strings ("1s", "0s", "200ms"). Same
//      fallback role as Anthropic's.
//
// When nothing parseable is present, callers use the configured
// default backoff (the design picks 30s; can be overridden per
// upstream).
//
// See DESIGN_WORKSTREAM_A.local.md §1.3.

// defaultRateLimitBackoff is the fallback when no header parses.
// The design (§1.3) suggests 30-60s; we pick 30s — short enough
// that a transient rate-limit doesn't park a key for too long, long
// enough that a real quota window has a chance to refresh.
const defaultRateLimitBackoff = 30 * time.Second

// parseRetryAfter extracts the duration after which the offending
// key should be considered usable again. Returns the parsed
// duration and true on success; (defaultRateLimitBackoff, false)
// when no header parsed.
//
// `now` is the timestamp the function uses to compute "duration
// from now" when the header carries an absolute time.
func parseRetryAfter(headers http.Header, body []byte, source string, now time.Time) (time.Duration, bool) {
	if d, ok := parseRetryAfterHeader(headers.Get("Retry-After"), now); ok {
		return d, true
	}
	switch source {
	case "anthropic":
		// Anthropic uses RFC 3339 reset timestamps. The most
		// restrictive (latest reset) wins — if either tokens or
		// requests is rate-limited, the key has to wait for both.
		var latest time.Time
		for _, h := range []string{"anthropic-ratelimit-tokens-reset", "anthropic-ratelimit-requests-reset"} {
			if v := headers.Get(h); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(latest) {
					latest = t
				}
			}
		}
		if !latest.IsZero() && latest.After(now) {
			return latest.Sub(now), true
		}
	case "openai":
		// OpenAI uses duration strings on the reset headers.
		var maxDur time.Duration
		for _, h := range []string{"x-ratelimit-reset-tokens", "x-ratelimit-reset-requests"} {
			if v := headers.Get(h); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d > maxDur {
					maxDur = d
				}
			}
		}
		if maxDur > 0 {
			return maxDur, true
		}
	}
	// Body inspection: some upstreams put a retry hint in the
	// JSON error body (Anthropic, panshi). Look for a `retry_after`
	// or `retry_after_ms` field at the top level or under `error`.
	if d, ok := parseRetryAfterBody(body); ok {
		return d, true
	}
	return defaultRateLimitBackoff, false
}

// parseRetryAfterHeader handles the standard Retry-After value:
// either a non-negative integer count of seconds, or an HTTP-date
// timestamp. Returns ok=false when neither shape parses.
func parseRetryAfterHeader(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	// Integer seconds.
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	// HTTP-date.
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// parseRetryAfterBody walks a JSON error envelope looking for
// retry-hint fields. Recognized shapes (defensive — many providers
// invent their own):
//
//   {"retry_after": 30}                  // seconds
//   {"retry_after_ms": 30000}            // milliseconds
//   {"error": {"retry_after": 30}}       // nested under error
//   {"error": {"retry_after_ms": 30000}} // ditto
//
// Returns (defaultRateLimitBackoff, false) on parse failure or
// missing field.
func parseRetryAfterBody(body []byte) (time.Duration, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, false
	}
	// Top-level.
	if d, ok := readRetryFieldsFrom(raw); ok {
		return d, true
	}
	// Nested under `error`.
	if errRaw, ok := raw["error"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(errRaw, &nested) == nil {
			if d, ok := readRetryFieldsFrom(nested); ok {
				return d, true
			}
		}
	}
	return 0, false
}

func readRetryFieldsFrom(m map[string]json.RawMessage) (time.Duration, bool) {
	if v, ok := m["retry_after"]; ok {
		var n float64
		if json.Unmarshal(v, &n) == nil && n > 0 {
			return time.Duration(n * float64(time.Second)), true
		}
	}
	if v, ok := m["retry_after_ms"]; ok {
		var n float64
		if json.Unmarshal(v, &n) == nil && n > 0 {
			return time.Duration(n * float64(time.Millisecond)), true
		}
	}
	return 0, false
}
