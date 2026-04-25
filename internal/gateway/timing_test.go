package gateway

import (
	"testing"
	"time"
)

// TestBuildTimingInfo_PartitionClosesUnderRandomizedSegments mirrors
// the §6 unit test from DESIGN_B1_timing.local.md: synthesize a
// snapshot whose six named values sum to less than total, and assert
// the partition closes (six values + Other == Total) with Other
// non-negative.
func TestBuildTimingInfo_PartitionClosesUnderRandomizedSegments(t *testing.T) {
	// Fixed seed via hardcoded values so the test is deterministic;
	// the property under test is partition closure, not coverage of
	// the random space.
	cases := []struct {
		name       string
		client     time.Duration
		translate  time.Duration
		toUpstream time.Duration
		processing time.Duration
		out        time.Duration
		toClient   time.Duration
		totalMs    int64
	}{
		{
			name:       "typical streaming distribution",
			client:     1 * time.Millisecond,
			translate:  3 * time.Millisecond,
			toUpstream: 12 * time.Millisecond,
			processing: 7800 * time.Millisecond,
			out:        65 * time.Millisecond,
			toClient:   218 * time.Millisecond,
			totalMs:    8100,
		},
		{
			name:    "all zero except total — every named segment 0, other absorbs total",
			totalMs: 100,
		},
		{
			name:       "exact partition with no other",
			client:     10 * time.Millisecond,
			translate:  20 * time.Millisecond,
			toUpstream: 30 * time.Millisecond,
			processing: 40 * time.Millisecond,
			out:        50 * time.Millisecond,
			toClient:   50 * time.Millisecond,
			totalMs:    200,
		},
		{
			name:    "zero total — degenerate but valid",
			totalMs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := map[timingSegment]time.Duration{
				segClientToMesh:       tc.client,
				segMeshTranslationIn:  tc.translate,
				segMeshToUpstream:     tc.toUpstream,
				segUpstreamProcessing: tc.processing,
				segMeshTranslationOut: tc.out,
				segMeshToClient:       tc.toClient,
			}
			ti := buildTimingInfo(snap, tc.totalMs)
			sum := ti.ClientToMesh + ti.MeshTranslationIn + ti.MeshToUpstream +
				ti.UpstreamProcessing + ti.MeshTranslationOut + ti.MeshToClient + ti.Other
			if sum != ti.Total {
				t.Errorf("partition broken: sum=%d total=%d info=%+v", sum, ti.Total, ti)
			}
			if ti.Other < 0 {
				t.Errorf("other=%d, must be non-negative", ti.Other)
			}
			if ti.Total != tc.totalMs {
				t.Errorf("total=%d, want %d", ti.Total, tc.totalMs)
			}
		})
	}
}

// TestBuildTimingInfo_ClampsOversizedSegment verifies that an out-of-
// order callback that produced a span larger than the request's total
// duration cannot push Other negative. The clamp is the design's
// guard against scheduler-induced clock skew (§7.1).
func TestBuildTimingInfo_ClampsOversizedSegment(t *testing.T) {
	snap := map[timingSegment]time.Duration{
		// Single oversized segment — somehow upstream_processing
		// reports more wall-clock than the request itself took.
		segUpstreamProcessing: 500 * time.Millisecond,
	}
	ti := buildTimingInfo(snap, 100) // total is only 100 ms

	if ti.UpstreamProcessing > ti.Total {
		t.Errorf("upstream_processing=%d, must not exceed total=%d", ti.UpstreamProcessing, ti.Total)
	}
	if ti.Other < 0 {
		t.Errorf("other=%d, must be non-negative even under oversized input", ti.Other)
	}
	sum := ti.ClientToMesh + ti.MeshTranslationIn + ti.MeshToUpstream +
		ti.UpstreamProcessing + ti.MeshTranslationOut + ti.MeshToClient + ti.Other
	if sum != ti.Total {
		t.Errorf("partition broken under clamp: sum=%d total=%d", sum, ti.Total)
	}
}

// TestBuildTimingInfo_NegativeSegmentTreatedAsZero verifies that a
// negative duration in the snapshot (clock skew between callback
// goroutines) is treated as zero, not as a negative contribution to
// the sum.
func TestBuildTimingInfo_NegativeSegmentTreatedAsZero(t *testing.T) {
	snap := map[timingSegment]time.Duration{
		segClientToMesh:       50 * time.Millisecond,
		segMeshToUpstream:     -10 * time.Millisecond, // skew
		segUpstreamProcessing: 30 * time.Millisecond,
	}
	ti := buildTimingInfo(snap, 100)

	if ti.MeshToUpstream != 0 {
		t.Errorf("mesh_to_upstream=%d, want 0 (negative input clamped)", ti.MeshToUpstream)
	}
	sum := ti.ClientToMesh + ti.MeshTranslationIn + ti.MeshToUpstream +
		ti.UpstreamProcessing + ti.MeshTranslationOut + ti.MeshToClient + ti.Other
	if sum != ti.Total {
		t.Errorf("partition broken with negative input: sum=%d total=%d", sum, ti.Total)
	}
}

// TestBuildTimingInfo_NegativeTotalClampedToZero verifies the total-
// side clamp. A negative total is implausible for a real request but
// the function must not propagate it.
func TestBuildTimingInfo_NegativeTotalClampedToZero(t *testing.T) {
	ti := buildTimingInfo(map[timingSegment]time.Duration{}, -50)
	if ti.Total != 0 {
		t.Errorf("total=%d, want 0 (negative input clamped)", ti.Total)
	}
	if ti.Other != 0 {
		t.Errorf("other=%d, want 0", ti.Other)
	}
}
