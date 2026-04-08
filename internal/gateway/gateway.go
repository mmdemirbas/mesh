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
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" {
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

	mux := http.NewServeMux()

	switch cfg.Mode {
	case ModeAnthropicToOpenAI:
		mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
			handleA2O(w, r, cfg, client, apiKey, log)
		})
	case ModeOpenAIToAnthropic:
		mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
			handleO2A(w, r, cfg, client, apiKey, log)
		})
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	})

	ln, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		state.Global.Update("gateway", cfg.Name, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", cfg.Bind, err)
	}
	defer ln.Close()

	state.Global.Update("gateway", cfg.Name, state.Listening, cfg.Bind)
	state.Global.UpdateBind("gateway", cfg.Name, ln.Addr().String())
	metrics := state.Global.GetMetrics("gateway", cfg.Name)
	metrics.StartTime.Store(time.Now().UnixNano())

	log.Info("Gateway started", "bind", ln.Addr(), "mode", cfg.Mode, "upstream", cfg.Upstream)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx) //nolint:errcheck
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		state.Global.Update("gateway", cfg.Name, state.Failed, err.Error())
		return err
	}

	state.Global.Delete("gateway", cfg.Name)
	state.Global.DeleteMetrics("gateway", cfg.Name)
	return nil
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
		writeAnthropicError(w, 400, "invalid JSON: "+err.Error())
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

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", cfg.Upstream, bytes.NewReader(oaiBody))
	if err != nil {
		writeAnthropicError(w, 500, "cannot create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		writeAnthropicError(w, 502, "upstream request failed")
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}
	defer upstreamResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(upstreamResp.Body, maxUpstreamResponseSize))
	if err != nil {
		writeAnthropicError(w, 502, "cannot read upstream response")
		return
	}

	if upstreamResp.StatusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(upstreamResp.StatusCode, cfg.Mode)
		writeAnthropicError(w, status, "upstream error")
		log.Warn("Upstream error", "status", upstreamResp.StatusCode, "body", string(respBody), "elapsed", time.Since(start))
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

	result, _ := json.Marshal(anthResp)
	metrics.BytesTx.Add(int64(len(result)))

	w.Header().Set("Content-Type", "application/json")
	w.Write(result) //nolint:errcheck

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
		writeOpenAIError(w, 400, "invalid JSON: "+err.Error())
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

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", cfg.Upstream, bytes.NewReader(anthBody))
	if err != nil {
		writeOpenAIError(w, 500, "cannot create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		upstreamReq.Header.Set("x-api-key", apiKey)
		upstreamReq.Header.Set("anthropic-version", "2023-06-01")
	}

	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		writeOpenAIError(w, 502, "upstream request failed")
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}
	defer upstreamResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(upstreamResp.Body, maxUpstreamResponseSize))
	if err != nil {
		writeOpenAIError(w, 502, "cannot read upstream response")
		return
	}

	if upstreamResp.StatusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(upstreamResp.StatusCode, cfg.Mode)
		writeOpenAIError(w, status, "upstream error")
		log.Warn("Upstream error", "status", upstreamResp.StatusCode, "body", string(respBody), "elapsed", time.Since(start))
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

	result, _ := json.Marshal(oaiResp)
	metrics.BytesTx.Add(int64(len(result)))

	w.Header().Set("Content-Type", "application/json")
	w.Write(result) //nolint:errcheck

	log.Info("Request completed",
		"model", clientModel,
		"prompt_tokens", oaiResp.Usage.PromptTokens,
		"completion_tokens", oaiResp.Usage.CompletionTokens,
		"elapsed", time.Since(start),
	)
}

