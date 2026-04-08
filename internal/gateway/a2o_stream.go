package gateway

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/mmdemirbas/mesh/internal/state"
)

// handleA2OStream handles Direction A streaming: client expects Anthropic SSE,
// upstream returns OpenAI SSE. Reads OpenAI SSE chunks from upstream and
// translates each to Anthropic SSE events.
func handleA2OStream(w http.ResponseWriter, r *http.Request, oaiReq *ChatCompletionRequest, cfg *GatewayCfg, client *http.Client, apiKey, clientModel string, metrics *state.Metrics, log *slog.Logger) {
	// TODO: implement real SSE pass-through translation
	_ = atomic.Value{} // suppress unused import
	writeAnthropicError(w, 501, "streaming not yet implemented")
}
