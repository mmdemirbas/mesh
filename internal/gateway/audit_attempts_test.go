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

// TestRecorder_FinalUpstreamAlwaysEmittedSingleAttempt is the
// REVIEW #5 regression. Previously `final_upstream` and
// `final_key_id` were gated behind `len(Attempts) > 1`, so the
// dominant single-attempt path produced audit rows without any
// (upstream, key) attribution. Operators couldn't correlate a single
// audited request to the upstream that served it without crawling
// headers. Now both fields are emitted whenever non-empty,
// regardless of attempt count; only the `attempts` array stays gated
// (it would just repeat the upstream info for single-attempt rows).
func TestRecorder_FinalUpstreamAlwaysEmittedSingleAttempt(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelMetadata)
	start := time.Now()
	id := rec.Request(RequestMeta{Gateway: "gw", StartTime: start}, []byte("{}"))
	rec.Response(id, ResponseMeta{
		Status:        200,
		Outcome:       OutcomeOK,
		StartTime:     start,
		EndTime:       start.Add(50 * time.Millisecond),
		Attempts:      []Attempt{{UpstreamName: "panshi", KeyID: "PANSHI_KEY:beef", Outcome: AttemptOK}},
		FinalUpstream: "panshi",
		FinalKeyID:    "PANSHI_KEY:beef",
	}, nil)
	_ = rec.Close()

	rows := readRows(t, rec.dir)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	resp := rows[1]
	if got := resp["final_upstream"]; got != "panshi" {
		t.Errorf("final_upstream = %v, want panshi (REVIEW #5: must emit on single-attempt rows)", got)
	}
	if got := resp["final_key_id"]; got != "PANSHI_KEY:beef" {
		t.Errorf("final_key_id = %v, want PANSHI_KEY:beef", got)
	}
	// Single-attempt rows still skip the redundant `attempts` array.
	if _, ok := resp["attempts"]; ok {
		t.Errorf("attempts array should be omitted on single-attempt rows: %v", resp["attempts"])
	}
}

// TestRecorder_AttemptsEmittedOnFallback pins that multi-attempt rows
// still emit the full `attempts` array alongside the final fields.
func TestRecorder_AttemptsEmittedOnFallback(t *testing.T) {
	t.Parallel()
	rec := newTestRecorder(t, LogLevelMetadata)
	start := time.Now()
	id := rec.Request(RequestMeta{Gateway: "gw", StartTime: start}, []byte("{}"))
	rec.Response(id, ResponseMeta{
		Status:    200,
		Outcome:   OutcomeOK,
		StartTime: start,
		EndTime:   start.Add(120 * time.Millisecond),
		Attempts: []Attempt{
			{UpstreamName: "primary", KeyID: "K1:abc1", Outcome: AttemptRateLimited, FallbackReason: "429"},
			{UpstreamName: "secondary", KeyID: "K2:def2", Outcome: AttemptOK},
		},
		FinalUpstream: "secondary",
		FinalKeyID:    "K2:def2",
	}, nil)
	_ = rec.Close()

	rows := readRows(t, rec.dir)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	resp := rows[1]
	att, ok := resp["attempts"].([]any)
	if !ok || len(att) != 2 {
		t.Fatalf("expected 2-element attempts array, got %T %v", resp["attempts"], resp["attempts"])
	}
	if resp["final_upstream"] != "secondary" || resp["final_key_id"] != "K2:def2" {
		t.Errorf("final_* fields wrong: upstream=%v key=%v", resp["final_upstream"], resp["final_key_id"])
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
