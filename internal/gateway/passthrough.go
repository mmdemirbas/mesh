package gateway

import (
	"bytes"
	"compress/gzip"
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
func handlePassthrough(w http.ResponseWriter, r *http.Request, cfg GatewayCfg, client *http.Client, apiKey string, recorder *Recorder, log *slog.Logger) {
	start := time.Now()
	metrics := state.Global.GetMetrics("gateway", cfg.Name)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writePassthroughError(w, cfg.ClientAPI, 413, "request body too large")
		return
	}
	metrics.BytesRx.Add(int64(len(body)))

	var peek peekRequest
	_ = json.Unmarshal(body, &peek)

	reqID := recorder.Request(RequestMeta{
		Gateway:   cfg.Name,
		Direction: cfg.Direction().String(),
		Model:     peek.Model,
		Stream:    peek.Stream,
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   r.Header,
		StartTime: start,
	}, body)

	upURL, err := buildUpstreamURL(cfg.Upstream, r.URL)
	if err != nil {
		writePassthroughError(w, cfg.ClientAPI, 500, "invalid upstream url")
		recorder.Response(reqID, ResponseMeta{Status: 500, Outcome: OutcomeError, StartTime: start, EndTime: time.Now()}, nil)
		return
	}

	ureq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytes.NewReader(body))
	if err != nil {
		writePassthroughError(w, cfg.ClientAPI, 500, "cannot create upstream request")
		recorder.Response(reqID, ResponseMeta{Status: 500, Outcome: OutcomeError, StartTime: start, EndTime: time.Now()}, nil)
		return
	}
	copyPassthroughRequestHeaders(ureq.Header, r.Header, cfg, apiKey)

	uresp, err := client.Do(ureq)
	if err != nil {
		if r.Context().Err() != nil {
			recorder.Response(reqID, ResponseMeta{Status: 499, Outcome: OutcomeClientCancelled, StartTime: start, EndTime: time.Now()}, nil)
			return
		}
		writePassthroughError(w, cfg.ClientAPI, 502, err.Error())
		recorder.Response(reqID, ResponseMeta{Status: 502, Outcome: OutcomeError, StartTime: start, EndTime: time.Now()}, nil)
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}
	defer func() { _ = uresp.Body.Close() }()

	copyPassthroughResponseHeaders(w.Header(), uresp.Header)

	if isSSEResponse(uresp) {
		streamPassthroughResponse(w, r, uresp, cfg, reqID, recorder, metrics, start, log)
		return
	}

	w.WriteHeader(uresp.StatusCode)
	respBody, err := io.ReadAll(io.LimitReader(uresp.Body, maxUpstreamResponseSize))
	if err != nil {
		recorder.Response(reqID, ResponseMeta{Status: uresp.StatusCode, Outcome: OutcomeError, StartTime: start, EndTime: time.Now(), Headers: uresp.Header}, respBody)
		log.Warn("Upstream body read failed", "error", err)
		return
	}
	n, _ := w.Write(respBody)
	metrics.BytesTx.Add(int64(n))

	outcome := OutcomeOK
	if uresp.StatusCode >= 400 {
		outcome = OutcomeError
	}
	auditBody := decodeForAudit(respBody, uresp.Header.Get("Content-Encoding"), log)
	usage := parseUsage(auditBody, cfg.UpstreamAPI)

	recorder.Response(reqID, ResponseMeta{
		Status:    uresp.StatusCode,
		Outcome:   outcome,
		Usage:     usage,
		StartTime: start,
		EndTime:   time.Now(),
		Headers:   uresp.Header,
	}, auditBody)

	log.Info("Passthrough completed",
		"model", peek.Model,
		"status", uresp.StatusCode,
		"bytes", n,
		"elapsed", time.Since(start),
	)
}

// streamPassthroughResponse forwards an SSE upstream response to the client
// byte-for-byte, flushing after every read. It tees a capped copy of the body
// to the audit recorder for post-mortem inspection. Event-level reassembly
// (reconstructed text, parsed Usage) is added in the streaming audit phase.
func streamPassthroughResponse(w http.ResponseWriter, r *http.Request, uresp *http.Response, cfg GatewayCfg, reqID RequestID, recorder *Recorder, metrics *state.Metrics, start time.Time, log *slog.Logger) {
	// Ensure no buffering-in-front-of-client middleware kicks in.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")
	w.WriteHeader(uresp.StatusCode)

	flusher, _ := w.(http.Flusher)

	var buf bytes.Buffer
	capped := false
	tmp := make([]byte, 32*1024)
	var totalBytes int64
	outcome := OutcomeOK

	for {
		n, err := uresp.Body.Read(tmp)
		if n > 0 {
			if _, werr := w.Write(tmp[:n]); werr != nil {
				outcome = OutcomeClientCancelled
				log.Debug("Client write failed mid-stream", "error", werr)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			totalBytes += int64(n)
			if !capped {
				remaining := int64(maxAuditBodyBytes) - int64(buf.Len())
				if remaining > 0 {
					take := int64(n)
					if take > remaining {
						take = remaining
						capped = true
					}
					buf.Write(tmp[:take])
				} else {
					capped = true
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if r.Context().Err() != nil {
				outcome = OutcomeClientCancelled
			} else {
				outcome = OutcomeError
			}
			log.Debug("Upstream read stopped", "error", err)
			break
		}
	}
	if capped && outcome == OutcomeOK {
		outcome = OutcomeTruncated
	}
	metrics.BytesTx.Add(totalBytes)

	auditBody := decodeForAudit(buf.Bytes(), uresp.Header.Get("Content-Encoding"), log)
	recorder.Response(reqID, ResponseMeta{
		Status:    uresp.StatusCode,
		Outcome:   outcome,
		StartTime: start,
		EndTime:   time.Now(),
		Headers:   uresp.Header,
	}, auditBody)

	log.Info("Passthrough stream completed",
		"status", uresp.StatusCode,
		"bytes", totalBytes,
		"outcome", outcome,
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

func copyPassthroughRequestHeaders(dst, src http.Header, cfg GatewayCfg, apiKey string) {
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
		switch cfg.UpstreamAPI {
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
// Upstreams often send Content-Encoding: gzip even for SSE responses (Anthropic
// does), and the tee captures those raw compressed bytes. We decompress a copy
// here so the JSONL rows are human-readable while the wire bytes the client
// saw remain untouched. Unsupported or malformed encodings fall back to the
// raw bytes with a warning.
func decodeForAudit(body []byte, encoding string, log *slog.Logger) []byte {
	if len(body) == 0 {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			log.Debug("audit gzip reader init failed", "error", err)
			return body
		}
		defer func() { _ = zr.Close() }()
		out, err := io.ReadAll(io.LimitReader(zr, maxAuditBodyBytes))
		if err != nil {
			log.Debug("audit gzip decode failed", "error", err)
			return body
		}
		return out
	default:
		// deflate/br/zstd: not decoded. The raw bytes are still captured so
		// the log is lossless even if not human-readable.
		return body
	}
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
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil
		}
		if r.Usage.InputTokens == 0 && r.Usage.OutputTokens == 0 {
			return nil
		}
		return &Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
	case APIOpenAI:
		var r struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil
		}
		if r.Usage.PromptTokens == 0 && r.Usage.CompletionTokens == 0 {
			return nil
		}
		return &Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens}
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

