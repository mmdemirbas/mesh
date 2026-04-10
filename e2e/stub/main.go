//go:build e2e

// Command stub is a tiny canned-response HTTP server used as the upstream
// for the mesh gateway end-to-end scenario. It is compiled with the "e2e"
// build tag so go build ./... never produces it outside the e2e toolchain.
//
// Endpoints:
//
//	GET  /healthz           200 ok
//	POST /openai/happy      canned OpenAI chat.completion JSON
//	POST /openai/stream     canned OpenAI SSE stream (2 deltas + done)
//	POST /anthropic/happy   canned Anthropic /v1/messages JSON
//	POST /anthropic/529     529 overloaded_error body
//	POST /openai/malformed  200 + invalid JSON body
//
// The stub listens on :8080 by default (override via STUB_ADDR).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	mux.HandleFunc("/openai/happy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		resp := map[string]any{
			"id":      "cmpl-stub-1",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stub-openai",
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "hello from the openai stub",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/openai/stream", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		emit := func(payload map[string]any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		emit(map[string]any{
			"id":      "cmpl-stub-stream",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "stub-openai",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "hello "},
			}},
		})
		emit(map[string]any{
			"id":      "cmpl-stub-stream",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "stub-openai",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": "world"},
			}},
		})
		emit(map[string]any{
			"id":      "cmpl-stub-stream",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "stub-openai",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	mux.HandleFunc("/anthropic/happy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		resp := map[string]any{
			"id":    "msg_stub_1",
			"type":  "message",
			"role":  "assistant",
			"model": "stub-claude",
			"content": []any{
				map[string]any{"type": "text", "text": "hello from the anthropic stub"},
			},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/anthropic/529", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	})

	mux.HandleFunc("/openai/malformed", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "this is {not} json [")
	})

	addr := ":8080"
	if v := os.Getenv("STUB_ADDR"); v != "" {
		addr = v
	}
	fmt.Fprintf(os.Stderr, "stub listening on %s\n", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "stub: %v\n", err)
		os.Exit(1)
	}
}
