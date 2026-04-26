package gateway

import (
	"fmt"
	"net/http"
	"time"
)

// Workstream A — health configuration + outcome recording.
//
// Two independent toggles per upstream:
//
//   - Passive health tracking: always on by default. Records the
//     outcome of every real request and degrades a key after N
//     consecutive failures. Hooks into the dispatch path's error
//     handling (A.4+).
//   - Active probing: opt-in. A goroutine per upstream fires a
//     known-cheap request at a configured interval, feeding the
//     same key-state machinery as a real request would. Lands in
//     A.3.
//
// See DESIGN_WORKSTREAM_A.local.md §1.2 (config) and §3 (state
// transitions).

// HealthCfg is the per-upstream health configuration. Zero value =
// passive enabled with defaults; active disabled.
type HealthCfg struct {
	Passive PassiveHealthCfg `yaml:"passive,omitempty"`
	Active  ActiveHealthCfg  `yaml:"active,omitempty"`
}

// PassiveHealthCfg controls "track real-request outcomes" health
// detection. Disabled is the inverted toggle so the zero value
// matches the design's "default enable" intent.
type PassiveHealthCfg struct {
	// Disabled turns off passive health tracking entirely. Default
	// (false) keeps it on. Operators set true when they want a key
	// pool to never auto-degrade — for example, when the upstream
	// behind the key is known-flaky and the operator prefers to
	// see the failures pass through to the client rather than
	// silently retry.
	Disabled bool `yaml:"disabled,omitempty"`
	// FailureThreshold is the number of consecutive failures
	// (5xx, network, timeout) that flips a key to degraded. A
	// successful response resets the consecutive-failure counter
	// to zero. Default: 3.
	FailureThreshold int `yaml:"failure_threshold,omitempty"`
	// Backoff is the duration a key stays degraded after the
	// threshold is crossed via passive observation. Distinct from
	// the rate-limit backoff (which uses the upstream's
	// retry-after header). Default: "30s".
	Backoff string `yaml:"backoff,omitempty"`
}

// ActiveHealthCfg is the active-probe block. Used by A.3.
// Defined here so A.2's validation can return an error if
// unrecognized fields appear in the YAML, and so the type round-
// trips through round-trip tests without surprise.
type ActiveHealthCfg struct {
	Enabled  bool   `yaml:"enabled,omitempty"`
	Interval string `yaml:"interval,omitempty"`
	Timeout  string `yaml:"timeout,omitempty"`
	// Payload is the JSON body sent on each probe. When unset,
	// A.3 picks a sensible default per the upstream's API shape.
	Payload string `yaml:"payload,omitempty"`
}

// Defaults applied when a field is unset.
const (
	defaultPassiveFailureThreshold = 3
	defaultPassiveBackoff          = 30 * time.Second
	defaultActiveProbeInterval     = 60 * time.Second
	defaultActiveProbeTimeout      = 10 * time.Second
)

// PassiveEnabled reports the effective enabled state of passive
// health tracking, applying the inverted-default rule.
func (h HealthCfg) PassiveEnabled() bool {
	return !h.Passive.Disabled
}

// PassiveFailureThreshold returns the effective threshold,
// substituting the default when the field is zero.
func (h HealthCfg) PassiveFailureThreshold() int {
	if h.Passive.FailureThreshold > 0 {
		return h.Passive.FailureThreshold
	}
	return defaultPassiveFailureThreshold
}

// PassiveBackoffDuration returns the effective backoff. Caller has
// already validated the string parses; on parse failure (defensive)
// the default is returned.
func (h HealthCfg) PassiveBackoffDuration() time.Duration {
	if h.Passive.Backoff == "" {
		return defaultPassiveBackoff
	}
	d, err := time.ParseDuration(h.Passive.Backoff)
	if err != nil || d <= 0 {
		return defaultPassiveBackoff
	}
	return d
}

// validate checks the per-upstream health block. Called from
// GatewayCfg.Validate() under the upstream loop.
func (h HealthCfg) validate(upstreamLabel string) error {
	if h.Passive.FailureThreshold < 0 {
		return fmt.Errorf("%s: health.passive.failure_threshold must be non-negative", upstreamLabel)
	}
	if h.Passive.Backoff != "" {
		if _, err := time.ParseDuration(h.Passive.Backoff); err != nil {
			return fmt.Errorf("%s: invalid health.passive.backoff %q: %w", upstreamLabel, h.Passive.Backoff, err)
		}
	}
	if h.Active.Interval != "" {
		if _, err := time.ParseDuration(h.Active.Interval); err != nil {
			return fmt.Errorf("%s: invalid health.active.interval %q: %w", upstreamLabel, h.Active.Interval, err)
		}
	}
	if h.Active.Timeout != "" {
		if _, err := time.ParseDuration(h.Active.Timeout); err != nil {
			return fmt.Errorf("%s: invalid health.active.timeout %q: %w", upstreamLabel, h.Active.Timeout, err)
		}
	}
	if h.Active.Enabled {
		if h.Active.Payload == "" {
			return fmt.Errorf("%s: health.active.enabled requires health.active.payload (mesh cannot pick a default model name across heterogeneous upstreams)", upstreamLabel)
		}
	}
	return nil
}

// classifyOutcome maps a (status, err) pair to an AttemptOutcome.
// 2xx → AttemptOK; 429 → AttemptRateLimited; 5xx → AttemptUpstreamError;
// other 4xx → AttemptClientError; non-nil err with no status →
// AttemptNetworkError.
func classifyOutcome(status int, err error) AttemptOutcome {
	if err != nil && status == 0 {
		return AttemptNetworkError
	}
	switch {
	case status >= 200 && status < 300:
		return AttemptOK
	case status == http.StatusTooManyRequests:
		return AttemptRateLimited
	case status >= 500:
		return AttemptUpstreamError
	case status >= 400:
		return AttemptClientError
	}
	// Non-2xx, non-4xx, non-5xx (e.g. 304, 1xx) is an unusual
	// shape; treat as ok so we don't degrade for protocol-level
	// pass-throughs.
	return AttemptOK
}

// recordPassiveOutcome applies the design's §3.1 state transition
// rules to a single key:
//
//   - AttemptOK: increment success counter, reset
//     consecutive-failure counter, clear any prior degradation.
//   - AttemptClientError (4xx other than 429): request shape
//     problem, not an upstream-health signal. Don't increment
//     failures; just bump lastUsed.
//   - AttemptRateLimited / AttemptUpstreamError /
//     AttemptNetworkError / AttemptTimeout: increment failures;
//     when consecutive failures reach the threshold, mark
//     degraded for the configured backoff window.
//
// Disabled passive tracking still updates counters (so the audit
// row reflects what really happened) but never degrades the key.
//
// A.4 (rate-limit) handles AttemptRateLimited's degradation via
// retry-after instead of the passive backoff; this function bumps
// the counter but A.4 will MarkDegraded with the upstream-supplied
// duration.
func recordPassiveOutcome(key *KeyState, hc HealthCfg, outcome AttemptOutcome, now time.Time) {
	if key == nil {
		return
	}
	switch outcome {
	case AttemptOK:
		key.MarkSuccess(now)
		return
	case AttemptClientError:
		key.MarkUsed(now)
		return
	}
	// All remaining outcomes are upstream-side failures.
	key.MarkFailure(now)
	if !hc.PassiveEnabled() {
		return
	}
	threshold := hc.PassiveFailureThreshold()
	if threshold <= 0 {
		return
	}
	// Atomic check-and-degrade. Must happen under a single lock,
	// otherwise a racing MarkSuccess between Snapshot and MarkDegraded
	// could degrade the key after its consecutive-failure streak just
	// ended. See REVIEW_WORKSTREAM_A.local.md #3.
	key.MarkDegradedIfConsecFailures(int64(threshold), now.Add(hc.PassiveBackoffDuration()))
}
