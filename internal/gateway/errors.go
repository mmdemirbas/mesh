package gateway

import (
	"encoding/json"
	"net/http"
)

// Error status mapping between Anthropic and OpenAI.

// anthropicToOpenAIStatus maps Anthropic HTTP status codes to OpenAI equivalents.
var anthropicToOpenAIStatus = map[int]int{
	400: 400,
	401: 401,
	402: 402,
	403: 403,
	404: 404,
	413: 413,
	429: 429,
	500: 500,
	529: 503, // Anthropic overloaded -> standard 503
}

// openaiToAnthropicStatus maps OpenAI HTTP status codes to Anthropic equivalents.
var openaiToAnthropicStatus = map[int]int{
	400: 400,
	401: 401,
	402: 402,
	403: 403,
	404: 404,
	413: 413,
	429: 429,
	500: 500,
	503: 529, // standard 503 -> Anthropic overloaded
}

// openaiStatusToErrorType maps HTTP status codes to OpenAI error type strings.
var openaiStatusToErrorType = map[int]string{
	400: "invalid_request_error",
	401: "authentication_error",
	403: "permission_error",
	404: "not_found_error",
	413: "request_too_large",
	429: "rate_limit_error",
	500: "server_error",
	503: "server_error",
}

// anthropicStatusToErrorType maps HTTP status codes to Anthropic error type strings.
var anthropicStatusToErrorType = map[int]string{
	400: "invalid_request_error",
	401: "authentication_error",
	402: "billing_error",
	403: "permission_error",
	404: "not_found_error",
	413: "request_too_large",
	429: "rate_limit_error",
	500: "api_error",
	529: "overloaded_error",
}

// writeOpenAIError writes an OpenAI-format error response.
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	errType := openaiStatusToErrorType[status]
	if errType == "" {
		errType = "server_error"
	}
	resp := OpenAIErrorResponse{
		Error: OpenAIError{
			Message: msg,
			Type:    errType,
		},
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeAnthropicError writes an Anthropic-format error response.
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	errType := anthropicStatusToErrorType[status]
	if errType == "" {
		errType = "api_error"
	}
	resp := AnthropicErrorResponse{
		Type: "error",
		Error: AnthropicError{
			Type:    errType,
			Message: msg,
		},
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// translateUpstreamErrorStatus maps an upstream HTTP error status to the
// client-facing status based on the translation direction. Same-API passthrough
// directions return the upstream status unchanged.
func translateUpstreamErrorStatus(upstreamStatus int, dir Direction) int {
	switch dir {
	case DirA2O:
		if mapped, ok := openaiToAnthropicStatus[upstreamStatus]; ok {
			return mapped
		}
	case DirO2A:
		if mapped, ok := anthropicToOpenAIStatus[upstreamStatus]; ok {
			return mapped
		}
	}
	return upstreamStatus
}

// translatedUpstreamErrorMessage includes the upstream error body in the
// translated client-facing error when available, while capping the size so the
// gateway never reflects arbitrarily large payloads.
func translatedUpstreamErrorMessage(body []byte) string {
	if len(body) == 0 {
		return "upstream error"
	}
	return "upstream error: " + truncateBody(body, 4096)
}

