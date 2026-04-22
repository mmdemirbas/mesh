package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

// hop-by-hop headers defined by RFC 7230 + proxy-specific. Not forwarded in
// either direction; a transparent proxy must not pass these along verbatim.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// peekRequest captures the minimum needed to populate audit metadata.
// Top-level `model` and `stream` appear in both Anthropic and OpenAI requests.
type peekRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// handlePassthrough forwards the client request to the upstream without
// translating the API format. It supports both buffered (JSON) and streamed
// (SSE) responses, preserving the upstream's status code and headers.
//
// Auth policy: when apiKey is non-empty (cfg.APIKeyEnv was set), the gateway
// overwrites the client's Authorization/x-api-key with the configured key.
// When apiKey is empty, client auth headers are preserved verbatim — this is
// required for OAuth-authenticated clients such as Claude Code.
func handlePassthrough(w http.ResponseWriter, r *http.Request, gwName, clientAPI string, upstream *ResolvedUpstream, dir Direction, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writePassthroughError(w, clientAPI, 413, "request body too large")
		return
	}

	var peek peekRequest
	_ = json.Unmarshal(body, &peek)

	upURL, err := buildUpstreamURL(upstream.Cfg.Target, r.URL)
	if err != nil {
		writePassthroughError(w, clientAPI, 500, "invalid upstream url")
		return
	}

	ureq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytes.NewReader(body))
	if err != nil {
		writePassthroughError(w, clientAPI, 500, "cannot create upstream request")
		return
	}
	copyPassthroughRequestHeaders(ureq.Header, r.Header, upstream.Cfg.API, upstream.APIKey)

	uresp, err := upstream.Client.Do(ureq)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writePassthroughError(w, clientAPI, 502, err.Error())
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}
	defer func() { _ = uresp.Body.Close() }()

	copyPassthroughResponseHeaders(w.Header(), uresp.Header)

	if isSSEResponse(uresp) {
		streamPassthroughResponse(w, r, uresp, metrics, start, log)
		return
	}

	w.WriteHeader(uresp.StatusCode)
	respBody, err := io.ReadAll(io.LimitReader(uresp.Body, maxUpstreamResponseSize))
	if err != nil {
		log.Warn("Upstream body read failed", "error", err)
		return
	}
	n, _ := w.Write(respBody)
	metrics.BytesTx.Add(int64(n))

	log.Info("Passthrough completed",
		"model", peek.Model,
		"status", uresp.StatusCode,
		"bytes", n,
		"elapsed", time.Since(start),
	)
}

// streamPassthroughResponse forwards an SSE upstream response to the client
// byte-for-byte, flushing after every read.
func streamPassthroughResponse(w http.ResponseWriter, r *http.Request, uresp *http.Response, metrics *state.Metrics, start time.Time, log *slog.Logger) {
	// Ensure no buffering-in-front-of-client middleware kicks in.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")
	w.WriteHeader(uresp.StatusCode)

	flusher, _ := w.(http.Flusher)

	tmp := make([]byte, 32*1024)
	var totalBytes int64

	for {
		n, err := uresp.Body.Read(tmp)
		if n > 0 {
			if _, werr := w.Write(tmp[:n]); werr != nil {
				log.Debug("Client write failed mid-stream", "error", werr)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			totalBytes += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Debug("Upstream read stopped", "error", err)
			break
		}
	}
	metrics.BytesTx.Add(totalBytes)

	log.Info("Passthrough stream completed",
		"status", uresp.StatusCode,
		"bytes", totalBytes,
		"elapsed", time.Since(start),
	)
}

// buildUpstreamURL combines the configured upstream base with the client's
// request URL. When cfg.Upstream includes a path (common for translation-mode
// configs), that path is used as-is — passthrough usage is expected to point
// at the API root (e.g., "https://api.anthropic.com") and let the client's
// request path flow through.
func buildUpstreamURL(upstream string, reqURL *url.URL) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return "", err
	}
	// Upstream has its own path (e.g., /v1/messages). Respect it and ignore
	// the client path — caller is in translation mode or wants a fixed
	// endpoint.
	if u.Path != "" && u.Path != "/" {
		return u.String(), nil
	}
	u.Path = reqURL.Path
	u.RawQuery = reqURL.RawQuery
	return u.String(), nil
}

func copyPassthroughRequestHeaders(dst, src http.Header, upstreamAPI, apiKey string) {
	for k, v := range src {
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			continue
		}
		if strings.EqualFold(k, "host") {
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
	if apiKey != "" {
		dst.Del("Authorization")
		dst.Del("X-Api-Key")
		switch upstreamAPI {
		case APIAnthropic:
			dst.Set("X-Api-Key", apiKey)
			if dst.Get("Anthropic-Version") == "" {
				dst.Set("Anthropic-Version", "2023-06-01")
			}
		case APIOpenAI:
			dst.Set("Authorization", "Bearer "+apiKey)
		}
	}
}

func copyPassthroughResponseHeaders(dst, src http.Header) {
	for k, v := range src {
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
}

func isSSEResponse(r *http.Response) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

// decodeForAudit returns a plaintext copy of body suitable for the audit log.
// Upstreams often send Content-Encoding even for SSE responses (Anthropic
// uses gzip), and the tee captures those raw compressed bytes. We decompress
// a copy here so the JSONL rows are human-readable while the wire bytes the
// client saw remain untouched. Unsupported or malformed encodings (br, zstd,
// or anything that fails to decode) fall back to the raw bytes — the log
// stays lossless even when not human-readable.
func decodeForAudit(body []byte, encoding string, log *slog.Logger) []byte {
	if len(body) == 0 {
		return body
	}
	enc := strings.ToLower(strings.TrimSpace(encoding))
	if enc == "" || enc == "identity" {
		return body
	}
	if out, ok := decodeCompressed(body, enc, log); ok {
		return out
	}
	return body
}

func decodeCompressed(body []byte, enc string, log *slog.Logger) ([]byte, bool) {
	var rdr io.ReadCloser
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			log.Debug("audit gzip reader init failed", "error", err)
			return nil, false
		}
		rdr = zr
	case "deflate":
		// HTTP "deflate" historically means zlib-wrapped flate; some servers
		// send raw flate. Try zlib first, then raw flate.
		if zr, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			rdr = zr
		} else {
			rdr = flate.NewReader(bytes.NewReader(body))
		}
	default:
		// br / zstd require third-party deps not present in this module.
		return nil, false
	}
	defer func() { _ = rdr.Close() }()
	out, err := io.ReadAll(io.LimitReader(rdr, maxAuditBodyBytes))
	if err != nil {
		log.Debug("audit decode failed", "encoding", enc, "error", err)
		return nil, false
	}
	return out, true
}

// parseUsage extracts token counts from a non-streaming response body. Returns
// nil when the body is not valid JSON or lacks a usage field.
func parseUsage(body []byte, upstreamAPI string) *Usage {
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	switch upstreamAPI {
	case APIAnthropic:
		var r struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil
		}
		u := &Usage{
			InputTokens:              r.Usage.InputTokens,
			OutputTokens:             r.Usage.OutputTokens,
			CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     r.Usage.CacheReadInputTokens,
		}
		if u.isZero() {
			return nil
		}
		return u
	case APIOpenAI:
		var r struct {
			Usage struct {
				PromptTokens            int `json:"prompt_tokens"`
				CompletionTokens        int `json:"completion_tokens"`
				CompletionTokensDetails struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil
		}
		u := &Usage{
			InputTokens:          r.Usage.PromptTokens,
			OutputTokens:         r.Usage.CompletionTokens,
			CacheReadInputTokens: r.Usage.PromptTokensDetails.CachedTokens,
			ReasoningTokens:      r.Usage.CompletionTokensDetails.ReasoningTokens,
		}
		if u.isZero() {
			return nil
		}
		return u
	}
	return nil
}

// writePassthroughError emits an error response in the client's expected
// format. Same-API passthrough still needs a sensible error shape when the
// gateway itself rejects the request (413 too large, 500 bad URL).
func writePassthroughError(w http.ResponseWriter, clientAPI string, status int, msg string) {
	switch clientAPI {
	case APIAnthropic:
		writeAnthropicError(w, status, msg)
	case APIOpenAI:
		writeOpenAIError(w, status, msg)
	default:
		http.Error(w, msg, status)
	}
}
