package gateway

import (
	"testing"
	"time"
)

func TestDeriveFinalUpstream(t *testing.T) {
	t.Parallel()
	if deriveFinalUpstream(nil) != "" {
		t.Errorf("nil → empty")
	}
	if deriveFinalUpstream([]Attempt{}) != "" {
		t.Errorf("empty → empty")
	}
	att := []Attempt{
		{UpstreamName: "primary", Outcome: AttemptRateLimited},
		{UpstreamName: "secondary", Outcome: AttemptOK},
	}
	if got := deriveFinalUpstream(att); got != "secondary" {
		t.Errorf("got %q, want secondary", got)
	}
}

func TestDeriveFinalKeyID(t *testing.T) {
	t.Parallel()
	att := []Attempt{
		{UpstreamName: "u", KeyID: "K1:abc1", Outcome: AttemptRateLimited},
		{UpstreamName: "u", KeyID: "K2:abc2", Outcome: AttemptOK},
	}
	if got := deriveFinalKeyID(att); got != "K2:abc2" {
		t.Errorf("got %q, want K2:abc2", got)
	}
}

func TestAttempt_Duration(t *testing.T) {
	t.Parallel()
	a := Attempt{
		StartedAt: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 4, 26, 12, 0, 5, 0, time.UTC),
	}
	if a.Duration() != 5*time.Second {
		t.Errorf("got %v, want 5s", a.Duration())
	}
	// Zero start/end → zero duration.
	if (Attempt{}).Duration() != 0 {
		t.Errorf("zero attempt duration should be 0")
	}
}
