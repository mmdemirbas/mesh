package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestB1TimingPartition_RatioMeetsCalibratedBaseline is the e2e tier
// for B1 §9 criterion 7 (DESIGN_B1_timing.local.md): on a non-
// pathological run, the named segments must absorb almost all of
// total — confirming the instrumentation actually fired rather than
// silently funneling work into `other`.
//
// Per the B1 lead-in, the threshold is calibrated against the actual
// measured ratio on this machine. Race detector is always on for
// task check, so the calibration uses -race numbers (the relaxed
// scheduling pushes more goroutine-wakeup time into `other`).
//
// Reality measured under -race across 8 runs:
//   - non-streaming: 0.962 typical, 0.943 worst-case outlier
//   - streaming:     0.980 typical, 0.974 worst-case
//   - other:         2–4 ms in both
//
// Without -race the same setup hits 0.98–1.00 with `other`=0–1 ms.
//
// The 0.90 floor sits a few points below the worst -race run (pin 2:
// "if reality is 96%, set 90%"). It comfortably absorbs scheduler
// jitter while still tripping on the failure mode the test exists
// to catch — instrumentation that ran but never fired, dumping
// segment work into `other`. A missing httptrace plumbing call or
// stripped Scan/Add bracket craters upstream_processing to 0 and
// drops the ratio below 0.10.
//
// The 5 ms `other` cap lifts the design's §3.3 number; under -race
// the 2–4 ms baseline already eats most of it. If it flaps we'll
// raise it then; until it does, hold the design contract.
//
// The upstream stubs use deliberate time.Sleep to inflate a single
// segment so the partition has integer-millisecond fidelity. The
// sleep is upstream pacing, not a test synchronization primitive
// (testing.md): the assertion polls audit rows on a deadline.
func TestB1TimingPartition_RatioMeetsCalibratedBaseline(t *testing.T) {
	t.Parallel()
	// 50 ms upstream pacing — large enough that millisecond
	// truncation across six segments cannot dominate the ratio.
	const upstreamDelay = 50 * time.Millisecond
	// Streaming chunk pacing — three chunks at this interval put
	// ~150 ms into the stream and dwarf any per-Scan or per-Write
	// overhead that lands outside the partition.
	const perChunkDelay = 50 * time.Millisecond
	// Calibrated thresholds. Measured baselines on this machine are
	// well above; thresholds sit a few points below to absorb noise
	// without letting a real instrumentation gap slip through.
	const minNamedRatio = 0.90
	const maxOtherMs = 5.0

	cases := []struct {
		name     string
		upstream func() *httptest.Server
		body     string
	}{
		{
			name: "non_streaming",
			upstream: func() *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					time.Sleep(upstreamDelay)
					resp := ChatCompletionResponse{
						ID: "x", Model: "glm-4.7",
						Choices: []OpenAIChoice{{
							Message:      OpenAIMsg{Role: "assistant", Content: json.RawMessage(`"hi"`)},
							FinishReason: "stop",
						}},
						Usage: &OpenAIUsage{PromptTokens: 5, CompletionTokens: 1},
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(resp)
				}))
			},
			body: `{"model":"claude-opus-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "streaming",
			upstream: func() *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					f := w.(http.Flusher)
					chunks := []string{
						`{"id":"x","model":"glm-4.7","choices":[{"delta":{"content":"a"},"finish_reason":null}]}`,
						`{"id":"x","model":"glm-4.7","choices":[{"delta":{"content":"b"},"finish_reason":null}]}`,
						`{"id":"x","model":"glm-4.7","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
					}
					for _, c := range chunks {
						time.Sleep(perChunkDelay)
						_, _ = w.Write([]byte("data: " + c + "\n\n"))
						f.Flush()
					}
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					f.Flush()
				}))
			},
			body: `{"model":"claude-opus-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := tc.upstream()
			defer upstream.Close()

			base, gwName, logDir := startTranslationGateway(t, APIAnthropic, APIOpenAI, upstream.URL)

			resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			dir := filepath.Join(logDir, gwName)
			rows := waitForRows(t, func() []map[string]any { return readRows(t, dir) }, 2, 3*time.Second)
			if len(rows) < 2 {
				t.Fatalf("audit rows = %d, want >= 2", len(rows))
			}
			respRow := rows[1]
			timing, ok := respRow["timing_ms"].(map[string]any)
			if !ok {
				t.Fatalf("timing_ms missing from resp row: %+v", respRow)
			}
			mt := func(k string) float64 { v, _ := timing[k].(float64); return v }

			named := mt("client_to_mesh") + mt("mesh_translation_in") +
				mt("mesh_to_upstream") + mt("upstream_processing") +
				mt("mesh_translation_out") + mt("mesh_to_client")
			total := mt("total")
			other := mt("other")

			if total <= 0 {
				t.Fatalf("total=%v, want > 0; timing=%+v", total, timing)
			}
			ratio := named / total
			t.Logf("measured: named=%v total=%v other=%v ratio=%.4f", named, total, other, ratio)

			if ratio < minNamedRatio {
				t.Errorf("named/total ratio = %.4f, want >= %.2f; timing=%+v", ratio, minNamedRatio, timing)
			}
			if other > maxOtherMs {
				t.Errorf("other = %v ms, want < %v ms; timing=%+v", other, maxOtherMs, timing)
			}
		})
	}
}
