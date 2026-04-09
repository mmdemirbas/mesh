package gateway

import "encoding/json"

// OpenAI Chat Completions API types.

// ChatCompletionRequest is the OpenAI POST /v1/chat/completions request body.
type ChatCompletionRequest struct {
	Model         string          `json:"model"`
	Messages      []OpenAIMsg     `json:"messages"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          json.RawMessage `json:"stop,omitempty"` // string or []string
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
	Tools         []OpenAITool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"` // string or object
	User          string          `json:"user,omitempty"`

	// Fields dropped when translating to Anthropic.
	N                *int            `json:"n,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	LogitBias        json.RawMessage `json:"logit_bias,omitempty"`
	Logprobs         *bool           `json:"logprobs,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
}

// StreamOptions controls streaming behavior.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// OpenAIMsg is a message in the OpenAI messages array.
type OpenAIMsg struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"` // string or []ContentPart
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// ContentPart is a typed content element in an OpenAI message.
type ContentPart struct {
	Type     string    `json:"type"` // "text" or "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL holds image data as a URL (including data URIs).
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// OpenAIToolCall represents a tool invocation in an assistant message.
type OpenAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"` // "function"
	Function OpenAIFuncCall `json:"function"`
}

// OpenAIFuncCall holds the function name and arguments.
type OpenAIFuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// OpenAITool defines a tool in the OpenAI format.
type OpenAITool struct {
	Type     string         `json:"type"` // "function"
	Function OpenAIFunction `json:"function"`
}

// OpenAIFunction holds function metadata and schema.
type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// OpenAIToolChoiceFunc is the object form of tool_choice.
type OpenAIToolChoiceFunc struct {
	Type     string                   `json:"type"` // "function"
	Function OpenAIToolChoiceFuncName `json:"function"`
}

// OpenAIToolChoiceFuncName holds just the name for tool_choice objects.
type OpenAIToolChoiceFuncName struct {
	Name string `json:"name"`
}

// ChatCompletionResponse is the OpenAI non-streaming response.
type ChatCompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

// OpenAIChoice represents a single completion choice.
type OpenAIChoice struct {
	Index        int       `json:"index"`
	Message      OpenAIMsg `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

// OpenAIUsage tracks token consumption.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIErrorResponse is the error envelope.
type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error"`
}

// OpenAIError holds error details.
type OpenAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// --- Streaming chunk types ---

// ChatCompletionChunk is a single SSE chunk in the OpenAI streaming response.
type ChatCompletionChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"` // "chat.completion.chunk"
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []OpenAIChunkChoice `json:"choices"`
	Usage   *OpenAIUsage        `json:"usage,omitempty"`
}

// OpenAIChunkChoice represents a delta update in a streaming chunk.
type OpenAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        OpenAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

// OpenAIChunkDelta holds incremental content in a streaming chunk.
type OpenAIChunkDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   *string               `json:"content,omitempty"`
	ToolCalls []OpenAIChunkToolCall `json:"tool_calls,omitempty"`
}

// OpenAIChunkToolCall is a partial tool call in a streaming chunk.
type OpenAIChunkToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"` // "function"
	Function OpenAIChunkFuncCall `json:"function"`
}

// OpenAIChunkFuncCall holds partial function data in a streaming chunk.
type OpenAIChunkFuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
