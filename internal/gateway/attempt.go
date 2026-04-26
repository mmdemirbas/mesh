package gateway

import "time"

// Workstream A — per-attempt record.
//
// An Attempt is one (upstream, key) pair tried for a request. The
// chain dispatcher (A.5) accumulates a slice of Attempt values per
// request and stamps them onto the audit row (A.6). The frontend
// (A.7) renders them as a "panshi → claude" path indicator.
//
// A.0 ships the type; A.4-A.6 populate it during dispatch. Until
// then no Attempt values are produced — existing audit rows are
// unchanged.

// AttemptOutcome classifies the result of one upstream attempt for
// fallback-decision and audit purposes. Not the same as the
// outermost request outcome (which is the OutcomeOK / OutcomeError
// enum on ResponseMeta).
type AttemptOutcome string

const (
	// AttemptOK — upstream returned 2xx. Request finalized.
	AttemptOK AttemptOutcome = "ok"
	// AttemptRateLimited — upstream returned 429 or equivalent.
	// Key marked degraded for the retry-after window; rotation
	// picks another key (or chain advances).
	AttemptRateLimited AttemptOutcome = "rate_limited"
	// AttemptUpstreamError — upstream returned 5xx.
	AttemptUpstreamError AttemptOutcome = "upstream_error"
	// AttemptClientError — upstream returned 4xx other than 429.
	// Typically means the request itself is malformed; advancing
	// the chain doesn't help. Default behavior is no-retry; see
	// retry_within_upstream_on_4xx_other config.
	AttemptClientError AttemptOutcome = "client_error"
	// AttemptNetworkError — TCP / TLS / DNS / context-cancel
	// before the upstream produced a status code.
	AttemptNetworkError AttemptOutcome = "network_error"
	// AttemptTimeout — request exceeded the per-upstream timeout
	// without producing a status code.
	AttemptTimeout AttemptOutcome = "timeout"
)

// Attempt is one upstream dispatch attempt. Fields are JSON-tagged
// for direct emission into the audit row's `attempts` array (A.6).
type Attempt struct {
	UpstreamName string         `json:"upstream_name"`
	KeyID        string         `json:"key_id,omitempty"`
	StartedAt    time.Time      `json:"started_at"`
	EndedAt      time.Time      `json:"ended_at"`
	Outcome      AttemptOutcome `json:"outcome"`
	StatusCode   int            `json:"status_code,omitempty"`
	Error        string         `json:"error,omitempty"`
	// FallbackReason names why the dispatcher advanced past this
	// attempt. Empty on the final (successful or final-failed)
	// attempt; non-empty on every preceding attempt that triggered
	// a chain step or key rotation.
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// Duration returns EndedAt - StartedAt. Zero when either field is
// unset.
func (a Attempt) Duration() time.Duration {
	if a.StartedAt.IsZero() || a.EndedAt.IsZero() {
		return 0
	}
	return a.EndedAt.Sub(a.StartedAt)
}
