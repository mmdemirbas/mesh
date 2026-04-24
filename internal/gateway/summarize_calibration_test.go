package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

// calibrationFixture is one of the synthetic request bodies in testdata/.
// The files are hand-built lorem-ipsum (no production data — see the
// README there); the calibration test treats them as the ground-truth
// anchor for the estimator's bias.
type calibrationFixture struct {
	name        string
	file        string
	description string
}

// tiktokenProxyFloor is the minimum acceptable ratio of the estimator's
// output to cl100k_base's token count on a fixture. Below 0.95 means we
// are under-counting by more than 5% — a hard failure in admission
// control, because the gateway would silently let a request past the
// upstream's context limit.
const tiktokenProxyFloor = 0.95

// tiktokenProxyCeiling is the maximum acceptable ratio. Above 1.25
// means we over-count by more than 25% — costly because it triggers
// summarization on requests that would have fit.
const tiktokenProxyCeiling = 1.25

// Why cl100k_base? Anthropic does not publish the Claude tokenizer, so
// we have no ground truth for Claude bodies. cl100k_base is the
// GPT-3.5/4 BPE encoding; it lands within single-digit percent of
// Claude tokenization on typical English+code mixes — good enough for
// admission-control sizing, not good enough for billing. Phase 1a audit
// rows will carry actual Claude usage from response payloads, and
// Phase 1b will re-validate this bound against real traffic.
//
// The fixtures under testdata/ are hand-synthesized (lorem ipsum + a
// tiny PNG), not captured from real traffic. They cover the shape of
// a typical Claude Code turn: system + tools + alternating user/
// assistant turns with thinking blocks + one large tool_result + one
// image.
func TestEstimateTokens_MatchesTiktokenWithinEnvelope(t *testing.T) {
	t.Parallel()

	enc, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		t.Fatalf("tokenizer.Get: %v", err)
	}

	fixtures := []calibrationFixture{
		{
			name:        "system_string",
			file:        "testdata/calibration_request.json",
			description: "Anthropic request with system as a string (most common Claude Code form)",
		},
		{
			name:        "system_block_array",
			file:        "testdata/calibration_request_system_array.json",
			description: "same body with system as a [{type:text,text:...}] block array (Anthropic's other form)",
		},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(filepath.Clean(fx.file))
			if err != nil {
				t.Fatalf("read %s: %v", fx.file, err)
			}

			est := estimateTokens(body)
			ids, _, err := enc.Encode(string(body))
			if err != nil {
				t.Fatalf("tiktoken encode: %v", err)
			}
			actual := len(ids)
			if actual == 0 {
				t.Fatal("tiktoken returned 0 tokens; fixture is empty or malformed")
			}
			ratio := float64(est) / float64(actual)

			t.Logf("fixture=%s body=%d estimate=%d tiktoken=%d ratio=%.3f",
				fx.name, len(body), est, actual, ratio)

			if ratio < tiktokenProxyFloor {
				t.Errorf("estimator under-counts: ratio=%.3f < %.2f floor "+
					"(est=%d, tiktoken=%d) — admission control would "+
					"silently exceed upstream context window",
					ratio, tiktokenProxyFloor, est, actual)
			}
			if ratio > tiktokenProxyCeiling {
				t.Errorf("estimator over-counts: ratio=%.3f > %.2f ceiling "+
					"(est=%d, tiktoken=%d) — summarization would fire on "+
					"requests that would fit upstream",
					ratio, tiktokenProxyCeiling, est, actual)
			}
		})
	}
}

// TestEstimateTokens_ConsistentAcrossSystemForms asserts that the two
// accepted shapes for `system` (string vs block-array) land within one
// scaffolding-byte's worth of each other. A large divergence would mean
// the estimator is systematically biased against one legitimate wire
// format, which is a bug regardless of whether both forms pass the
// main calibration envelope.
func TestEstimateTokens_ConsistentAcrossSystemForms(t *testing.T) {
	t.Parallel()
	bString, err := os.ReadFile(filepath.Clean("testdata/calibration_request.json"))
	if err != nil {
		t.Fatalf("read string form: %v", err)
	}
	bArray, err := os.ReadFile(filepath.Clean("testdata/calibration_request_system_array.json"))
	if err != nil {
		t.Fatalf("read array form: %v", err)
	}

	estString := estimateTokens(bString)
	estArray := estimateTokens(bArray)

	// The only difference between the two fixtures is the wrapping of
	// `system` as an array of text blocks. The estimate for the array
	// form should be >= string form (more JSON scaffolding), and within
	// 5% — otherwise the estimator has a wider bias against one form.
	if estArray < estString {
		t.Errorf("block-array form estimated LOWER than string form "+
			"(array=%d, string=%d); whole-body estimator should count "+
			"the extra JSON scaffolding", estArray, estString)
	}
	diff := float64(estArray-estString) / float64(estString)
	if diff > 0.05 {
		t.Errorf("block-array form diverges from string form by %.1f%% "+
			"(array=%d, string=%d); expected <5%%", diff*100, estArray, estString)
	}
}
