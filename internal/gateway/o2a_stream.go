package gateway

import (
	"log/slog"
	"net/http"

	"github.com/mmdemirbas/mesh/internal/state"
)

// handleO2AStream handles Direction B streaming: client expects OpenAI SSE,
// upstream returns Anthropic SSE. Reads Anthropic SSE events from upstream and
// translates each to OpenAI SSE chunks.
func handleO2AStream(w http.ResponseWriter, r *http.Request, anthReq *MessagesRequest, cfg *GatewayCfg, client *http.Client, apiKey, clientModel string, oaiReq *ChatCompletionRequest, metrics *state.Metrics, log *slog.Logger) {
	// TODO: implement real SSE pass-through translation
	writeOpenAIError(w, 501, "streaming not yet implemented")
}
