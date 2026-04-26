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
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/nodeutil"
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

// Start launches a compound gateway HTTP server. It creates one HTTP server
// per client bind, all sharing the same router and audit recorder.
// It blocks until ctx is cancelled.
func Start(ctx context.Context, cfg GatewayCfg, log *slog.Logger) error {
	log = log.With("gateway", cfg.Name)

	router, err := NewRouter(&cfg, log)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	// Warn about empty API key env vars.
	var emptyKeyWarnings []string
	for _, u := range cfg.Upstream {
		if u.APIKeyEnv != "" {
			ru := router.Upstream(u.Name)
			if ru != nil && ru.APIKey == "" {
				emptyKeyWarnings = append(emptyKeyWarnings, u.APIKeyEnv)
				log.Warn("API key env var is empty", "upstream", u.Name, "var", u.APIKeyEnv)
			}
		}
	}

	recorder, err := NewRecorder(cfg.Name, cfg.Log, log)
	if err != nil {
		return fmt.Errorf("audit log init: %w", err)
	}
	defer func() { _ = recorder.Close() }()

	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	// Workstream A.3: launch active health probes for every
	// upstream that opted in. Goroutines join the same WaitGroup
	// as the client listeners so Start blocks on their exit too —
	// pre-fix they were fire-and-forget (deep-review M1).
	runActiveProbes(ctx, router, &wg, log)

	for _, cl := range cfg.Client {
		cl := cl
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer nodeutil.RecoverPanic("gateway.startClientListener")
			if err := startClientListener(ctx, cfg, cl, router, recorder, emptyKeyWarnings, log); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}()
	}

	wg.Wait()
	return firstErr
}

// startClientListener starts an HTTP server for a single client bind address.
func startClientListener(ctx context.Context, cfg GatewayCfg, cl ClientCfg, router *Router, recorder *Recorder, emptyKeyWarnings []string, log *slog.Logger) error {
	compKey := cfg.Name + "/" + cl.Bind
	log = log.With("bind", cl.Bind, "api", cl.API)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Build a routing handler that peeks the model name from the body,
	// resolves the upstream, determines the direction, and dispatches.
	routingHandler := buildRoutingHandler(cfg, cl, router, recorder, log)

	// Register the appropriate paths based on client API.
	switch cl.API {
	case APIAnthropic:
		mux.HandleFunc("POST /v1/messages", routingHandler)
	case APIOpenAI:
		mux.HandleFunc("POST /v1/chat/completions", routingHandler)
	}

	ln, err := net.Listen("tcp", cl.Bind)
	if err != nil {
		state.Global.Update("gateway", compKey, state.Failed, err.Error())
		return fmt.Errorf("listen %s: %w", cl.Bind, err)
	}
	defer func() { _ = ln.Close() }()

	listenMsg := ""
	if len(emptyKeyWarnings) > 0 {
		listenMsg = emptyKeyWarnings[0] + " is empty"
	}
	state.Global.Update("gateway", compKey, state.Listening, listenMsg)
	state.Global.UpdateBind("gateway", compKey, ln.Addr().String())
	metrics := state.Global.GetMetrics("gateway", compKey)
	metrics.StartTime.Store(time.Now().UnixNano())

	log.Info("Gateway client started", "bind", ln.Addr(), "client_api", cl.API)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		state.Global.Update("gateway", compKey, state.Failed, err.Error())
		return err
	}

	state.Global.Delete("gateway", compKey)
	state.Global.DeleteMetrics("gateway", compKey)
	return nil
}

// buildRoutingHandler creates an HTTP handler that peeks the model from the
// request body, routes to the appropriate upstream, and dispatches to the
// correct translation/passthrough handler based on direction.
func buildRoutingHandler(cfg GatewayCfg, cl ClientCfg, router *Router, recorder *Recorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		compKey := cfg.Name + "/" + cl.Bind
		metrics := state.Global.GetMetrics("gateway", compKey)

		// Read and buffer the body so we can peek the model name, then replay.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
		if err != nil {
			writeClientError(cl.API, w, 413, "request body too large")
			return
		}
		metrics.BytesRx.Add(int64(len(body)))

		// Peek model from body.
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &peek)

		// A.5 — resolve to a chain instead of a single upstream.
		// The first chain element is the "primary"; later
		// elements are fallbacks the dispatch wrapper walks on
		// per-upstream failure.
		chain := router.RouteChain(peek.Model)
		if len(chain) == 0 {
			chain = router.DefaultChain()
		}
		if len(chain) == 0 {
			writeClientError(cl.API, w, 404, "no upstream found for model: "+peek.Model)
			return
		}
		upstream := chain[0]

		// Replay the body for the inner handler.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		dir := ResolveDirection(cl.API, upstream.Cfg.API)

		// Audit wrapping (if recorder is non-nil).
		// The inner handler runs after wrapAuditing has captured the original
		// body, so summarization here does not affect what the audit log records.
		innerHandler := func(w http.ResponseWriter, r *http.Request) {
			// A.5: stash the chain on the request context so
			// handleA2O / handleO2A and the streaming handlers can
			// walk it via dispatchAcrossChain or per-step chain
			// loops. Pre-fix this rode on au.Chain only, so
			// streaming gateways without a recorder silently lost
			// chain semantics (deep-review I2).
			r = withChain(r, chain)
			if au := getAuditUpstream(r); au != nil {
				au.Chain = chain
			}
			// Context window check — only for Anthropic client API where we
			// can parse and reconstruct the message array.
			if cl.API == APIAnthropic && upstream.Cfg.HasContextLimit() {
				innerBody, _ := io.ReadAll(r.Body)
				newBody, result, info, err := checkAndSummarize(r.Context(), innerBody, upstream, router, log)
				if au := getAuditUpstream(r); au != nil {
					au.ContextWindowTokens = upstream.Cfg.ContextWindow
					au.OriginalInputTokensEstimate = info.OriginalTokens
					au.EffectiveInputTokensEstimate = info.EffectiveTokens
					au.Summarized = info.Summarized
					au.SummarizeBytesRemoved = info.BytesRemoved
					au.SummarizeBytesAdded = info.BytesAdded
					au.SummarizeTurnsCollapsed = info.TurnsCollapsed
				}
				switch result {
				case contextExceeded:
					writeClientError(cl.API, w, 413, err.Error())
					return
				case contextError:
					writeClientError(cl.API, w, 502, err.Error())
					return
				case contextSummarized:
					log.Info("Request summarized for upstream",
						"original_tokens", info.OriginalTokens,
						"effective_tokens", info.EffectiveTokens,
						"new_size", len(newBody),
						"context_window", upstream.Cfg.ContextWindow)
					innerBody = newBody
				}
				r.Body = io.NopCloser(bytes.NewReader(innerBody))
				r.ContentLength = int64(len(innerBody))
			}
			dispatchRequest(w, r, cfg, cl, upstream, dir, metrics, log)
		}

		wrappedHandler := wrapAuditing(cfg.Name, &upstream.Cfg, cl.API, recorder, router.readIdx, http.HandlerFunc(innerHandler))
		wrappedHandler.ServeHTTP(w, r)
	}
}

// dispatchRequest sends the request to the correct handler based on direction.
func dispatchRequest(w http.ResponseWriter, r *http.Request, cfg GatewayCfg, cl ClientCfg, upstream *ResolvedUpstream, dir Direction, metrics *state.Metrics, log *slog.Logger) {
	switch dir {
	case DirA2O:
		handleA2O(w, r, cfg.Name, upstream, metrics, log)
	case DirO2A:
		handleO2A(w, r, cfg.Name, upstream, metrics, log)
	case DirA2A, DirO2O:
		handlePassthrough(w, r, cfg.Name, cl.API, upstream, dir, metrics, log)
	}
}

// writeClientError writes an error in the client's expected API format.
func writeClientError(clientAPI string, w http.ResponseWriter, status int, msg string) {
	if clientAPI == APIAnthropic {
		writeAnthropicError(w, status, msg)
	} else {
		writeOpenAIError(w, status, msg)
	}
}

// doUpstreamRequest sends body to upstreamURL via POST, applying Content-Type and
// any extra headers, and returns the response body. Thin shim over
// doUpstreamRequestFull that drops the response headers; preserved
// for the dispatch sites that don't need rate-limit parsing.
func doUpstreamRequest(ctx context.Context, client *http.Client, upstreamURL string, body []byte, extraHeaders map[string]string, log *slog.Logger) (statusCode int, respBody []byte, err error) {
	statusCode, _, respBody, err = doUpstreamRequestFull(ctx, client, upstreamURL, body, extraHeaders, log)
	return
}

// doUpstreamRequestFull is the rate-limit-aware variant: returns
// the response headers alongside the status and body so callers can
// parse Retry-After / x-ratelimit-* / anthropic-ratelimit-* hints
// (A.4). Behaviorally identical to doUpstreamRequest otherwise.
func doUpstreamRequestFull(ctx context.Context, client *http.Client, upstreamURL string, body []byte, extraHeaders map[string]string, log *slog.Logger) (statusCode int, respHeaders http.Header, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("cannot create upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Error("Upstream request failed", "error", err)
		return 0, nil, nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseSize))
	if err != nil {
		return 0, resp.Header, nil, fmt.Errorf("cannot read upstream response: %w", err)
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// handleA2O handles client=Anthropic, upstream=OpenAI translation.
func handleA2O(w http.ResponseWriter, r *http.Request, gwName string, upstream *ResolvedUpstream, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writeAnthropicError(w, 413, "request body too large")
		return
	}

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, 400, "invalid json: "+err.Error())
		return
	}

	clientModel := req.Model

	oaiReq, err := translateAnthropicRequest(&req, &upstream.Cfg)
	if err != nil {
		writeAnthropicError(w, 400, "translation error: "+err.Error())
		return
	}

	if req.Stream {
		handleA2OStream(w, r, oaiReq, upstream, clientModel, metrics, log)
		return
	}

	oaiBody, _ := json.Marshal(oaiReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = oaiBody
	}

	ctx := r.Context()
	au := getAuditUpstream(r)
	sessionID := ""
	if au != nil {
		ctx = attachTimingTrace(ctx, au.Timer, au.ReqID)
		sessionID = au.SessionID
	}
	// I2: read chain from request context (independent of audit).
	chain := chainFromRequest(r)
	if len(chain) == 0 {
		chain = []*ResolvedUpstream{upstream}
	}
	// A.4/A.5 — dispatch with multi-key rotation across the
	// configured chain. For single-key single-upstream configs
	// the wrapper makes a single attempt equivalent to the
	// legacy doUpstreamRequest call.
	res := dispatchAcrossChain(ctx, chain, oaiBody, dispatchOpts{
		SessionID: sessionID,
	}, log)
	if au != nil {
		au.Attempts = res.Attempts
	}
	statusCode, respBody, err := res.StatusCode, res.Body, res.Err
	if err != nil {
		writeAnthropicError(w, 502, err.Error())
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}

	// Record the raw upstream response body for the audit log.
	if au != nil {
		au.RespBody = respBody
	}

	if statusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(statusCode, DirA2O)
		writeAnthropicError(w, status, translatedUpstreamErrorMessage(respBody))
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

// handleO2A handles client=OpenAI, upstream=Anthropic translation.
func handleO2A(w http.ResponseWriter, r *http.Request, gwName string, upstream *ResolvedUpstream, metrics *state.Metrics, log *slog.Logger) {
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		writeOpenAIError(w, 413, "request body too large")
		return
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, 400, "invalid json: "+err.Error())
		return
	}

	clientModel := req.Model

	anthReq, err := translateOpenAIRequest(&req, &upstream.Cfg)
	if err != nil {
		writeOpenAIError(w, 400, "translation error: "+err.Error())
		return
	}

	if req.Stream {
		handleO2AStream(w, r, anthReq, upstream, clientModel, &req, metrics, log)
		return
	}

	anthBody, _ := json.Marshal(anthReq)

	// Record the upstream request body for the audit log.
	if au := getAuditUpstream(r); au != nil {
		au.ReqBody = anthBody
	}

	ctx := r.Context()
	au := getAuditUpstream(r)
	sessionID := ""
	if au != nil {
		ctx = attachTimingTrace(ctx, au.Timer, au.ReqID)
		sessionID = au.SessionID
	}
	// I2: read chain from request context (independent of audit).
	chain := chainFromRequest(r)
	if len(chain) == 0 {
		chain = []*ResolvedUpstream{upstream}
	}
	res := dispatchAcrossChain(ctx, chain, anthBody, dispatchOpts{
		SessionID: sessionID,
	}, log)
	if au != nil {
		au.Attempts = res.Attempts
	}
	statusCode, respBody, err := res.StatusCode, res.Body, res.Err
	if err != nil {
		writeOpenAIError(w, 502, err.Error())
		log.Error("Upstream request failed", "error", err, "elapsed", time.Since(start))
		return
	}

	// Record the raw upstream response body for the audit log.
	if au != nil {
		au.RespBody = respBody
	}

	if statusCode != http.StatusOK {
		status := translateUpstreamErrorStatus(statusCode, DirO2A)
		writeOpenAIError(w, status, translatedUpstreamErrorMessage(respBody))
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
