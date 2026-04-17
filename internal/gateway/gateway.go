package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
)

const (
	// maxRequestBodySize caps incoming request bodies (32 MB).
	maxRequestBodySize = 32 * 1024 * 1024
	// maxUpstreamResponseSize caps non-streaming upstream response bodies (64 MB).
	maxUpstreamResponseSize = 64 * 1024 * 1024
	// maxSSELineSize caps individual SSE lines during streaming (4 MB).
	maxSSELineSize = 4 * 1024 * 1024
)

// Start launches a gateway HTTP server that translates between Anthropic and
// OpenAI API formats. It blocks until ctx is cancelled.
func Start(ctx context.Context, cfg GatewayCfg, log *slog.Logger) error {
	log = log.With("gateway", cfg.Name)

	apiKey := ""
	apiKeyEmpty := false
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" {
			apiKeyEmpty = true
			log.Warn("API key env var is empty", "var", cfg.APIKeyEnv)
		}
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	// Optional proxy for upstream (HTTP, HTTPS, SOCKS5).
	if cfg.Proxy != "" {
		proxyURL, err := url.Parse(cfg.Proxy)
		if err != nil {
			return fmt.Errorf("invalid proxy URL %q: %w", cfg.Proxy, err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.TimeoutDuration(),
	}

	recorder, err := NewRecorder(cfg, log)
	if err != nil {
		return fmt.Errorf("audit log init: %w", err)
	}
	defer func() { _ = recorder.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	dir := cfg.Direction()
	switch dir {
	case DirA2O:
		mux.HandleFunc("POST /v1/messages", wrapAuditing(cfg, recorder, APIAnthropic, func(w http.ResponseWriter, r *http.Request) {
			handleA2O(w, r, cfg, client, apiKey, log)
		}))
	case DirO2A:
		mux.HandleFunc("POST /v1/chat/completions", wrapAuditing(cfg, recorder, APIOpenAI, func(w http.ResponseWriter, r *http.Request) {
			handleO2A(w, r, cfg, client, apiKey, log)
		}))
	case DirA2A, DirO2O:
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			handlePassthrough(w, r, cfg, client, apiKey, recorder, log)
		})
	}

	ln, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		state.Global.Update("gateway", cfg.Name, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", cfg.Bind, err)
	}
	defer func() { _ = ln.Close() }()

	listenMsg := ""
	if apiKeyEmpty {
		listenMsg = cfg.APIKeyEnv + " is empty"
	}
	state.Global.Update("gateway", cfg.Name, state.Listening, listenMsg)
	state.Global.UpdateBind("gateway", cfg.Name, ln.Addr().String())
	metrics := state.Global.GetMetrics("gateway", cfg.Name)
	metrics.StartTime.Store(time.Now().UnixNano())

	log.Info("Gateway started", "bind", ln.Addr(), "direction", dir, "client_api", cfg.ClientAPI, "upstream_api", cfg.UpstreamAPI, "upstream", cfg.Upstream)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout caps the time a slow client can spend uploading the
		// 32 MB request body — without it a slowloris-style client can hold
		// a goroutine indefinitely after the headers complete.
		ReadTimeout: 2 * time.Minute,
		// WriteTimeout deliberately omitted: SSE streaming responses can run
		// for the duration of the upstream LLM call (minutes for long
		// generations) and the per-stream context cancels on client
		// disconnect.
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		state.Global.Update("gateway", cfg.Name, state.Failed, err.Error())
		return err
	}

	state.Global.Delete("gateway", cfg.Name)
	state.Global.DeleteMetrics("gateway", cfg.Name)
	return nil
}

// doUpstreamRequest sends body to upstreamURL via POST, applying Content-Type and
// any extra headers, and returns the response body. The caller owns the response
// status code; doUpstreamRequest only returns an error on transport failure or
// a body read failure. extraHeaders is applied after Content-Type so callers can
// override it if needed (though in practice they do not).
func doUpstreamRequest(ctx context.Context, client *http.Client, upstreamURL string, body []byte, extraHeaders map[string]string, log *slog.Logger) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("cannot create upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Error("Upstream request failed", "error", err)
		return 0, nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseSize))
	if err != nil {
		return 0, nil, fmt.Errorf("cannot read upstream response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// handleA2O handles Direction A: client sends Anthropic, gateway forwards as OpenAI.
func handleA2O(w http.ResponseWriter, r *http.Request, cfg GatewayCfg, client *http.Client, apiKey string, log *slog.Logger) {
	metrics := state.Global.GetMetrics("gateway", cfg.Name)
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writeAnthropicError(w, 413, "request body too large")
		return
	}
	metrics.BytesRx.Add(int64(len(body)))

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, 400, "invalid json: "+err.Error())
		return
	}

	clientModel := req.Model

	oaiReq, err := translateAnthropicRequest(&req, &cfg)
	if err != nil {
		writeAnthropicError(w, 400, "translation error: "+err.Error())
		return
	}

	if req.Stream {
		handleA2OStream(w, r, oaiReq, &cfg, client, apiKey, clientModel, metrics, log)
		return
	}

	oaiBody, _ := json.Marshal(oaiReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = oaiBody
	}

	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}

	statusCode, respBody, err := doUpstreamRequest(r.Context(), client, cfg.Upstream, oaiBody, headers, log)
	if err != nil {
		writeAnthropicError(w, 502, err.Error())
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}

	// Record the raw upstream response body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.RespBody = respBody
	}

	if statusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(statusCode, cfg.Direction())
		writeAnthropicError(w, status, "upstream error")
		log.Warn("Upstream error", "status", statusCode, "body", truncateBody(respBody, 512), "elapsed", time.Since(start))
		return
	}

	var oaiResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		writeAnthropicError(w, 502, "cannot parse upstream response: "+err.Error())
		return
	}

	anthResp, err := translateOpenAIResponse(&oaiResp, clientModel)
	if err != nil {
		writeAnthropicError(w, 502, "response translation error: "+err.Error())
		return
	}

	result, err := json.Marshal(anthResp)
	if err != nil {
		writeAnthropicError(w, 500, "response serialization error")
		log.Error("Failed to marshal response", "error", err)
		return
	}
	metrics.BytesTx.Add(int64(len(result)))

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)

	log.Info("Request completed",
		"model", clientModel,
		"input_tokens", anthResp.Usage.InputTokens,
		"output_tokens", anthResp.Usage.OutputTokens,
		"elapsed", time.Since(start),
	)
}

// handleO2A handles Direction B: client sends OpenAI, gateway forwards as Anthropic.
func handleO2A(w http.ResponseWriter, r *http.Request, cfg GatewayCfg, client *http.Client, apiKey string, log *slog.Logger) {
	metrics := state.Global.GetMetrics("gateway", cfg.Name)
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writeOpenAIError(w, 413, "request body too large")
		return
	}
	metrics.BytesRx.Add(int64(len(body)))

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, 400, "invalid json: "+err.Error())
		return
	}

	clientModel := req.Model

	anthReq, err := translateOpenAIRequest(&req, &cfg)
	if err != nil {
		writeOpenAIError(w, 400, "translation error: "+err.Error())
		return
	}

	if req.Stream {
		handleO2AStream(w, r, anthReq, &cfg, client, apiKey, clientModel, &req, metrics, log)
		return
	}

	anthBody, _ := json.Marshal(anthReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = anthBody
	}

	headers := map[string]string{}
	if apiKey != "" {
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	}

	statusCode, respBody, err := doUpstreamRequest(r.Context(), client, cfg.Upstream, anthBody, headers, log)
	if err != nil {
		writeOpenAIError(w, 502, err.Error())
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}

	// Record the raw upstream response body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.RespBody = respBody
	}

	if statusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(statusCode, cfg.Direction())
		writeOpenAIError(w, status, "upstream error")
		log.Warn("Upstream error", "status", statusCode, "body", truncateBody(respBody, 512), "elapsed", time.Since(start))
		return
	}

	var anthResp MessagesResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		writeOpenAIError(w, 502, "cannot parse upstream response: "+err.Error())
		return
	}

	oaiResp, err := translateAnthropicResponse(&anthResp, clientModel)
	if err != nil {
		writeOpenAIError(w, 502, "response translation error: "+err.Error())
		return
	}

	result, err := json.Marshal(oaiResp)
	if err != nil {
		writeOpenAIError(w, 500, "response serialization error")
		log.Error("Failed to marshal response", "error", err)
		return
	}
	metrics.BytesTx.Add(int64(len(result)))

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)

	log.Info("Request completed",
		"model", clientModel,
		"prompt_tokens", oaiResp.Usage.PromptTokens,
		"completion_tokens", oaiResp.Usage.CompletionTokens,
		"elapsed", time.Since(start),
	)
}

// truncateBody returns a string of at most maxLen bytes from body,
// appending "...(truncated)" if truncation occurred.
func truncateBody(body []byte, maxLen int) string {
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "...(truncated)"
}
