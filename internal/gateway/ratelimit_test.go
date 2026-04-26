package gateway

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter_HeaderInteger(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Retry-After", "60")
	now := time.Now()
	d, ok := parseRetryAfter(h, nil, "anthropic", now)
	if !ok || d != 60*time.Second {
		t.Errorf("got (%v,%v), want (60s,true)", d, ok)
	}
}

func TestParseRetryAfter_HeaderHTTPDate(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(120 * time.Second)
	h.Set("Retry-After", resetAt.UTC().Format(http.TimeFormat))
	d, ok := parseRetryAfter(h, nil, "openai", now)
	if !ok {
		t.Fatalf("expected http-date to parse")
	}
	// Allow ±1s slack for http.TimeFormat second-precision.
	if d < 119*time.Second || d > 121*time.Second {
		t.Errorf("got %v, want ~120s", d)
	}
}

func TestParseRetryAfter_AnthropicReset(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	h.Set("anthropic-ratelimit-tokens-reset", now.Add(45*time.Second).Format(time.RFC3339))
	h.Set("anthropic-ratelimit-requests-reset", now.Add(30*time.Second).Format(time.RFC3339))
	d, ok := parseRetryAfter(h, nil, "anthropic", now)
	if !ok {
		t.Fatalf("expected anthropic reset to parse")
	}
	// Most-restrictive (latest) wins.
	if d < 44*time.Second || d > 46*time.Second {
		t.Errorf("got %v, want ~45s (latest reset)", d)
	}
}

func TestParseRetryAfter_OpenAIReset(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-ratelimit-reset-tokens", "20s")
	h.Set("x-ratelimit-reset-requests", "5s")
	d, ok := parseRetryAfter(h, nil, "openai", time.Now())
	if !ok {
		t.Fatalf("expected openai reset to parse")
	}
	if d != 20*time.Second {
		t.Errorf("got %v, want 20s (max)", d)
	}
}

func TestParseRetryAfter_BodyJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body string
		want time.Duration
	}{
		{`{"retry_after": 30}`, 30 * time.Second},
		{`{"retry_after_ms": 5000}`, 5 * time.Second},
		{`{"error": {"retry_after": 12}}`, 12 * time.Second},
		{`{"error": {"retry_after_ms": 100}}`, 100 * time.Millisecond},
	}
	for _, c := range cases {
		d, ok := parseRetryAfter(http.Header{}, []byte(c.body), "openai", time.Now())
		if !ok {
			t.Errorf("parse failed for %q", c.body)
			continue
		}
		if d != c.want {
			t.Errorf("body %q: got %v, want %v", c.body, d, c.want)
		}
	}
}

func TestParseRetryAfter_FallbackDefault(t *testing.T) {
	t.Parallel()
	d, ok := parseRetryAfter(http.Header{}, nil, "openai", time.Now())
	if ok {
		t.Errorf("expected ok=false on no signal, got %v", d)
	}
	if d != defaultRateLimitBackoff {
		t.Errorf("default = %v, want %v", d, defaultRateLimitBackoff)
	}
}

func TestParseRetryAfter_HeaderTakesPrecedence(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Retry-After", "10")
	h.Set("anthropic-ratelimit-tokens-reset", time.Now().Add(time.Hour).Format(time.RFC3339))
	d, ok := parseRetryAfter(h, []byte(`{"retry_after": 999}`), "anthropic", time.Now())
	if !ok || d != 10*time.Second {
		t.Errorf("Retry-After should win; got %v ok=%v", d, ok)
	}
}
