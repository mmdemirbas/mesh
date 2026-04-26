package gateway

import (
	"errors"
	"testing"
	"time"
)

func TestHealthCfg_PassiveDefaults(t *testing.T) {
	t.Parallel()
	var h HealthCfg
	if !h.PassiveEnabled() {
		t.Errorf("zero HealthCfg should enable passive tracking")
	}
	if h.PassiveFailureThreshold() != defaultPassiveFailureThreshold {
		t.Errorf("default threshold = %d, want %d", h.PassiveFailureThreshold(), defaultPassiveFailureThreshold)
	}
	if h.PassiveBackoffDuration() != defaultPassiveBackoff {
		t.Errorf("default backoff = %v, want %v", h.PassiveBackoffDuration(), defaultPassiveBackoff)
	}
}

func TestHealthCfg_PassiveOverrides(t *testing.T) {
	t.Parallel()
	h := HealthCfg{Passive: PassiveHealthCfg{
		Disabled:         true,
		FailureThreshold: 7,
		Backoff:          "120s",
	}}
	if h.PassiveEnabled() {
		t.Errorf("Disabled=true should disable passive")
	}
	if h.PassiveFailureThreshold() != 7 {
		t.Errorf("threshold override not honored: got %d", h.PassiveFailureThreshold())
	}
	if h.PassiveBackoffDuration() != 120*time.Second {
		t.Errorf("backoff override not honored: got %v", h.PassiveBackoffDuration())
	}
}

func TestHealthCfg_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		h       HealthCfg
		wantErr string
	}{
		{"zero", HealthCfg{}, ""},
		{"valid_passive", HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 5, Backoff: "60s"}}, ""},
		{"negative_threshold", HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: -1}}, "non-negative"},
		{"bad_backoff", HealthCfg{Passive: PassiveHealthCfg{Backoff: "huge"}}, "invalid health.passive.backoff"},
		{"bad_active_interval", HealthCfg{Active: ActiveHealthCfg{Interval: "every"}}, "invalid health.active.interval"},
		{"bad_active_timeout", HealthCfg{Active: ActiveHealthCfg{Timeout: "soon"}}, "invalid health.active.timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.h.validate("test")
			if c.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if c.wantErr != "" && (err == nil || !contains(err.Error(), c.wantErr)) {
				t.Errorf("error %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestClassifyOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		err    error
		want   AttemptOutcome
	}{
		{200, nil, AttemptOK},
		{201, nil, AttemptOK},
		{429, nil, AttemptRateLimited},
		{500, nil, AttemptUpstreamError},
		{503, nil, AttemptUpstreamError},
		{400, nil, AttemptClientError},
		{404, nil, AttemptClientError},
		{0, errors.New("connection refused"), AttemptNetworkError},
		{304, nil, AttemptOK}, // unusual non-2xx, non-4xx, non-5xx
	}
	for _, c := range cases {
		got := classifyOutcome(c.status, c.err)
		if got != c.want {
			t.Errorf("classifyOutcome(%d, %v) = %q, want %q", c.status, c.err, got, c.want)
		}
	}
}

func TestRecordPassiveOutcome_DegradesAtThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	hc := HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 3, Backoff: "60s"}}
	k := NewKeyState("E", "v")

	// Two failures: not degraded yet.
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	if !k.IsUsable(now) {
		t.Errorf("key should still be usable after 2 failures (threshold=3)")
	}
	// Third failure: crosses threshold.
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	if k.IsUsable(now) {
		t.Errorf("key should be degraded after threshold crossed")
	}
	// Recovers after the backoff window.
	if !k.IsUsable(now.Add(61 * time.Second)) {
		t.Errorf("key should recover after backoff window")
	}
}

func TestRecordPassiveOutcome_SuccessResetsConsec(t *testing.T) {
	t.Parallel()
	now := time.Now()
	hc := HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 3}}
	k := NewKeyState("E", "v")
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	recordPassiveOutcome(k, hc, AttemptOK, now)
	// Now two more failures shouldn't degrade — the success reset
	// the consec counter.
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	if !k.IsUsable(now) {
		t.Errorf("key should still be usable; success between bursts resets consec failures")
	}
}

func TestRecordPassiveOutcome_DisabledNeverDegrades(t *testing.T) {
	t.Parallel()
	now := time.Now()
	hc := HealthCfg{Passive: PassiveHealthCfg{Disabled: true, FailureThreshold: 1}}
	k := NewKeyState("E", "v")
	for i := 0; i < 10; i++ {
		recordPassiveOutcome(k, hc, AttemptUpstreamError, now)
	}
	if !k.IsUsable(now) {
		t.Errorf("key should be usable when passive tracking disabled")
	}
	// Counters still update so audit reflects reality.
	if k.Snapshot().ConsecFailures != 10 {
		t.Errorf("consec failures = %d, want 10", k.Snapshot().ConsecFailures)
	}
}

func TestRecordPassiveOutcome_ClientErrorIsNotFailure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	hc := HealthCfg{Passive: PassiveHealthCfg{FailureThreshold: 1}}
	k := NewKeyState("E", "v")
	for i := 0; i < 5; i++ {
		recordPassiveOutcome(k, hc, AttemptClientError, now)
	}
	if !k.IsUsable(now) {
		t.Errorf("4xx should not degrade — request shape problem, not upstream health")
	}
	if k.Snapshot().Failures != 0 {
		t.Errorf("failures should not increment on 4xx, got %d", k.Snapshot().Failures)
	}
}

func TestRecordPassiveOutcome_NilSafe(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil key panicked: %v", r)
		}
	}()
	recordPassiveOutcome(nil, HealthCfg{}, AttemptUpstreamError, time.Now())
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
