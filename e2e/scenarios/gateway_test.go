//go:build e2e

package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/e2e/harness"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestGateway walks the LLM gateway end-to-end using a stub upstream that
// returns canned responses in both Anthropic and OpenAI formats. The
// scenario covers:
//
//   - Anthropic-to-OpenAI happy path (non-streaming)
//   - OpenAI-to-Anthropic happy path (non-streaming)
//   - Error translation: upstream 529 → client 503
//   - Malformed upstream body → client 500
//   - Anthropic-to-OpenAI streaming (SSE round trip)
//
// Gateways bind to 127.0.0.1 inside the mesh container per config
// validation, so client curls run via `docker exec` from the test.
func TestGateway(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	// Start the stub first so its alias is registered in docker DNS.
	// STUB_KEY is passed through to mesh just so api_key_env resolves.
	stub := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "stub",
		SkipConfig: true,
		Entrypoint: []string{"/usr/local/bin/stub-llm"},
		Env:        map[string]string{"STUB_ADDR": ":8080"},
		WaitFor: wait.ForExec([]string{"sh", "-c",
			"wget -q -O- http://127.0.0.1:8080/healthz >/dev/null"}).
			WithStartupTimeout(30 * time.Second).
			WithPollInterval(250 * time.Millisecond),
	})

	gwCfg, err := harness.LoadTemplate("configs/s4-gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// gw starts after stub so DNS resolution for "stub" works immediately.
	gwScript := "IP=$(getent hosts stub | awk '{print $1}'); " +
		"if [ -z \"$IP\" ]; then echo 'resolve stub failed' >&2; exit 1; fi; " +
		"sed -i \"s/STUB_IP/$IP/g\" /root/.mesh/conf/mesh.yaml && " +
		"exec /usr/local/bin/mesh -f /root/.mesh/conf/mesh.yaml up gw"

	gw := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "gw",
		Config:     gwCfg,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", gwScript},
		Env:        map[string]string{"STUB_KEY": "sk-stub-ignored"},
	})
	harness.DumpOnFailure(t, stub, gw)

	// Shared helpers ---------------------------------------------------

	// curlJSON runs curl inside gw, POSTs bodyJSON to the given local
	// gateway path, and returns (status, body).
	curlJSON := func(port int, path, bodyJSON string) (int, string) {
		t.Helper()
		cmd := []string{
			"curl", "-sS",
			"-o", "/tmp/resp.body",
			"-w", "%{http_code}",
			"-H", "Content-Type: application/json",
			"--data-binary", bodyJSON,
			fmt.Sprintf("http://127.0.0.1:%d%s", port, path),
		}
		out := gw.MustExec(ctx, cmd...)
		status := 0
		_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &status)
		body := gw.MustExec(ctx, "cat", "/tmp/resp.body")
		return status, body
	}

	// Phase 1 — Anthropic-to-OpenAI happy path. Client sends an
	// Anthropic-style /v1/messages request; stub returns canned OpenAI;
	// gateway translates back to Anthropic; client sees Anthropic shape.
	anthropicReq := `{"model":"claude-opus","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	status, body := curlJSON(3101, "/v1/messages", anthropicReq)
	if status != 200 {
		t.Fatalf("a2o-happy: status=%d body=%s", status, body)
	}
	var anthResp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &anthResp); err != nil {
		t.Fatalf("a2o-happy decode: %v body=%s", err, body)
	}
	if anthResp.Type != "message" || anthResp.Role != "assistant" {
		t.Fatalf("a2o-happy: wrong shape type=%q role=%q body=%s", anthResp.Type, anthResp.Role, body)
	}
	if len(anthResp.Content) == 0 || !strings.Contains(anthResp.Content[0].Text, "hello from the openai stub") {
		t.Fatalf("a2o-happy: text not found: %s", body)
	}

	// Phase 2 — OpenAI-to-Anthropic happy path. Client sends OpenAI
	// /v1/chat/completions; stub returns canned Anthropic; gateway
	// translates back to OpenAI; client sees chat.completion shape.
	openaiReq := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	status, body = curlJSON(3102, "/v1/chat/completions", openaiReq)
	if status != 200 {
		t.Fatalf("o2a-happy: status=%d body=%s", status, body)
	}
	var oaiResp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &oaiResp); err != nil {
		t.Fatalf("o2a-happy decode: %v body=%s", err, body)
	}
	if oaiResp.Object != "chat.completion" || len(oaiResp.Choices) == 0 {
		t.Fatalf("o2a-happy: wrong shape object=%q choices=%d body=%s", oaiResp.Object, len(oaiResp.Choices), body)
	}
	if !strings.Contains(oaiResp.Choices[0].Message.Content, "hello from the anthropic stub") {
		t.Fatalf("o2a-happy: content not found: %s", body)
	}

	// Phase 3 — error translation. Upstream returns Anthropic 529
	// overloaded_error; client (OpenAI-facing) sees 503.
	status, body = curlJSON(3103, "/v1/chat/completions", openaiReq)
	if status != 503 {
		t.Fatalf("o2a-529: status=%d want=503 body=%s", status, body)
	}

	// Phase 4 — malformed upstream body → 502 at the client. The gateway
	// reports upstream parse failures as Bad Gateway with an Anthropic-
	// shaped error body (see gateway.go writeAnthropicError).
	status, body = curlJSON(3104, "/v1/messages", anthropicReq)
	if status != 502 {
		t.Fatalf("a2o-malformed: status=%d want=502 body=%s", status, body)
	}
	if !strings.Contains(body, "cannot parse upstream response") {
		t.Fatalf("a2o-malformed: unexpected error body: %s", body)
	}

	// Phase 5 — streaming SSE. Client sets stream=true; stub streams
	// canned OpenAI chunks; gateway re-emits as Anthropic SSE events.
	streamReq := `{"model":"claude-opus","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	streamCmd := []string{
		"curl", "-sS", "-N",
		"-H", "Content-Type: application/json",
		"-H", "Accept: text/event-stream",
		"--data-binary", streamReq,
		"http://127.0.0.1:3105/v1/messages",
	}
	streamOut := gw.MustExec(ctx, streamCmd...)
	if !strings.Contains(streamOut, "event:") || !strings.Contains(streamOut, "data:") {
		t.Fatalf("a2o-stream: no SSE frames in output: %q", streamOut)
	}
	// The Anthropic streaming protocol emits message_start, then
	// content_block_delta frames, then message_stop. We are not
	// validating the full protocol here; the gateway unit tests do that.
	// We only prove the HTTP path forwards events end-to-end.
	if !strings.Contains(streamOut, "message_start") {
		t.Fatalf("a2o-stream: missing message_start in SSE output: %q", streamOut)
	}
}
