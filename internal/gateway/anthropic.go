package gateway

import "encoding/json"

// Anthropic Messages API types.

// MessagesRequest is the Anthropic POST /v1/messages request body.
type MessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []AnthropicMsg  `json:"messages"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []AnthropicTool `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Metadata      *AnthropicMeta  `json:"metadata,omitempty"`
}

// AnthropicMsg is a message in the Anthropic messages array.
type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

// ContentBlock is a typed content element within an Anthropic message.
type ContentBlock struct {
	Type string `json:"type"`

	// type: "text"
	Text string `json:"text,omitempty"`

	// type: "image"
	Source *ImageSource `json:"source,omitempty"`

	// type: "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type: "tool_result"
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []ContentBlock
	IsError   bool            `json:"is_error,omitempty"`

	// type: "thinking" — dropped during translation
	Thinking string `json:"thinking,omitempty"`

	// cache_control — stripped during translation
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// ImageSource holds base64 or URL image data.
type ImageSource struct {
	Type      string `json:"type"`       // "base64" or "url"
	MediaType string `json:"media_type"` // e.g., "image/png"
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool defines a tool in the Anthropic format.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicToolChoice controls tool selection behavior.
type AnthropicToolChoice struct {
	Type string `json:"type"`           // "auto", "any", "tool", "none"
	Name string `json:"name,omitempty"` // only for type="tool"
}

// AnthropicMeta holds request metadata.
type AnthropicMeta struct {
	UserID string `json:"user_id,omitempty"`
}

// MessagesResponse is the Anthropic non-streaming response.
type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"` // always "message"
	Role       string         `json:"role"` // always "assistant"
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason,omitempty"`
	Usage      AnthropicUsage `json:"usage"`
}

// AnthropicUsage tracks token consumption.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicErrorResponse is the error envelope.
type AnthropicErrorResponse struct {
	Type  string         `json:"type"` // "error"
	Error AnthropicError `json:"error"`
}

// AnthropicError holds error details.
type AnthropicError struct {
	Type    string `json:"type"` // e.g., "invalid_request_error"
	Message string `json:"message"`
}

// --- Streaming event types ---

// AnthropicStreamEvent represents a single SSE event from the Anthropic API.
type AnthropicStreamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *MessagesResponse `json:"message,omitempty"`

	// content_block_start
	Index        *int          `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`

	// content_block_delta
	Delta *AnthropicDelta `json:"delta,omitempty"`

	// message_delta
	Usage *AnthropicUsage `json:"usage,omitempty"`

	// error event
	Error *AnthropicError `json:"error,omitempty"`
}

// AnthropicDelta holds incremental content within a streaming event.
type AnthropicDelta struct {
	Type        string `json:"type,omitempty"`         // "text_delta", "input_json_delta", "thinking_delta"
	Text        string `json:"text,omitempty"`         // for text_delta
	Thinking    string `json:"thinking,omitempty"`     // for thinking_delta
	PartialJSON string `json:"partial_json,omitempty"` // for input_json_delta
	StopReason  string `json:"stop_reason,omitempty"`  // for message_delta
}
