package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// translateOpenAIRequest converts an OpenAI ChatCompletionRequest to an
// Anthropic MessagesRequest. Returns the original client model so the
// response can echo it back.
func translateOpenAIRequest(req *ChatCompletionRequest, cfg *GatewayCfg) (*MessagesRequest, error) {
	out := &MessagesRequest{
		Model:  cfg.MapModel(req.Model),
		Stream: req.Stream,
	}

	// max_tokens: required in Anthropic.
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = *req.MaxTokens
	} else {
		out.MaxTokens = cfg.MaxTokens()
	}

	// Temperature: clamp 0-2 -> 0-1.
	if req.Temperature != nil {
		t := *req.Temperature
		if t > 1.0 {
			t = 1.0
		}
		out.Temperature = &t
	}

	if req.TopP != nil {
		out.TopP = req.TopP
	}

	// Stop sequences.
	if len(req.Stop) > 0 {
		stops, err := parseStopSequences(req.Stop)
		if err != nil {
			return nil, err
		}
		out.StopSequences = stops
	}

	// User -> metadata.
	if req.User != "" {
		out.Metadata = &AnthropicMeta{UserID: req.User}
	}

	// Extract system messages and convert the rest.
	system, msgs, err := convertOpenAIMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if system != "" {
		out.System = json.RawMessage(mustMarshal(system))
	}
	out.Messages = msgs

	// Tools.
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			out.Tools = append(out.Tools, AnthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	// Tool choice.
	if len(req.ToolChoice) > 0 {
		tc, err := translateOpenAIToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// parseStopSequences handles both string and []string forms.
func parseStopSequences(raw json.RawMessage) ([]string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("cannot parse stop: %w", err)
	}
	return arr, nil
}

// convertOpenAIMessages extracts system messages and converts remaining
// messages to Anthropic format with strict user/assistant alternation.
func convertOpenAIMessages(msgs []OpenAIMsg) (string, []AnthropicMsg, error) {
	var systemParts []string
	var anthropicMsgs []AnthropicMsg

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			text := extractOpenAITextContent(m.Content)
			if text != "" {
				systemParts = append(systemParts, text)
			}

		case "user":
			blocks, err := convertOpenAIUserContent(m)
			if err != nil {
				return "", nil, err
			}
			anthropicMsgs = appendMerging(anthropicMsgs, "user", blocks)

		case "assistant":
			blocks, err := convertOpenAIAssistantContent(m)
			if err != nil {
				return "", nil, err
			}
			anthropicMsgs = appendMerging(anthropicMsgs, "assistant", blocks)

		case "tool":
			// Tool result goes into a user message as a tool_result block.
			block := ContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
			}
			text := extractOpenAITextContent(m.Content)
			if text != "" {
				block.Content = json.RawMessage(mustMarshal(text))
			}
			anthropicMsgs = appendMerging(anthropicMsgs, "user", []ContentBlock{block})
		}
	}

	return strings.Join(systemParts, "\n\n"), anthropicMsgs, nil
}

// appendMerging adds blocks to the message list, merging with the last message
// if it has the same role (Anthropic requires strict alternation).
func appendMerging(msgs []AnthropicMsg, role string, blocks []ContentBlock) []AnthropicMsg {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
		// Merge into existing message.
		var existing []ContentBlock
		if err := json.Unmarshal(msgs[len(msgs)-1].Content, &existing); err != nil {
			// Prior content was a string, not an array — wrap it as a text block.
			var s string
			if json.Unmarshal(msgs[len(msgs)-1].Content, &s) == nil {
				existing = []ContentBlock{{Type: "text", Text: s}}
			}
		}
		existing = append(existing, blocks...)
		msgs[len(msgs)-1].Content = json.RawMessage(mustMarshal(existing))
		return msgs
	}
	return append(msgs, AnthropicMsg{
		Role:    role,
		Content: json.RawMessage(mustMarshal(blocks)),
	})
}

// convertOpenAIUserContent converts a user message's content to Anthropic blocks.
func convertOpenAIUserContent(m OpenAIMsg) ([]ContentBlock, error) {
	if len(m.Content) == 0 {
		return []ContentBlock{{Type: "text", Text: ""}}, nil
	}

	// Try string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}

	// Try array of parts.
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, fmt.Errorf("cannot parse user content: %w", err)
	}

	var blocks []ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, ContentBlock{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				block, err := convertImageURL(p.ImageURL.URL)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			}
		}
	}
	return blocks, nil
}

// convertImageURL parses a data URI or plain URL into an Anthropic image block.
func convertImageURL(url string) (ContentBlock, error) {
	if strings.HasPrefix(url, "data:") {
		// Parse data:image/png;base64,<data>
		mediaType, data, err := parseDataURI(url)
		if err != nil {
			return ContentBlock{}, err
		}
		return ContentBlock{
			Type: "image",
			Source: &ImageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      data,
			},
		}, nil
	}
	return ContentBlock{
		Type: "image",
		Source: &ImageSource{
			Type: "url",
			URL:  url,
		},
	}, nil
}

// parseDataURI extracts media type and base64 data from a data URI.
func parseDataURI(uri string) (mediaType, data string, err error) {
	// data:image/png;base64,iVBOR...
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", "", fmt.Errorf("invalid data URI")
	}
	mediaType, data, ok = strings.Cut(rest, ";base64,")
	if !ok {
		return "", "", fmt.Errorf("data URI missing ;base64, marker")
	}
	return mediaType, data, nil
}

// convertOpenAIAssistantContent converts an assistant message to Anthropic blocks.
func convertOpenAIAssistantContent(m OpenAIMsg) ([]ContentBlock, error) {
	var blocks []ContentBlock

	// Text content.
	if len(m.Content) > 0 {
		text := extractOpenAITextContent(m.Content)
		if text != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: text})
		}
	}

	// Tool calls.
	for _, tc := range m.ToolCalls {
		var input json.RawMessage
		if tc.Function.Arguments != "" {
			// Validate it's valid JSON.
			if json.Valid([]byte(tc.Function.Arguments)) {
				input = json.RawMessage(tc.Function.Arguments)
			} else {
				input = json.RawMessage("{}")
			}
		} else {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	if len(blocks) == 0 {
		blocks = append(blocks, ContentBlock{Type: "text", Text: ""})
	}

	return blocks, nil
}

// extractOpenAITextContent extracts text from an OpenAI content field (string or array).
func extractOpenAITextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// translateOpenAIToolChoice converts OpenAI tool_choice to Anthropic format.
func translateOpenAIToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	// Try string form first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return json.Marshal(AnthropicToolChoice{Type: "auto"})
		case "none":
			return json.Marshal(AnthropicToolChoice{Type: "none"})
		case "required":
			return json.Marshal(AnthropicToolChoice{Type: "any"})
		default:
			return json.Marshal(AnthropicToolChoice{Type: "auto"})
		}
	}

	// Try object form.
	var obj OpenAIToolChoiceFunc
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("cannot parse tool_choice: %w", err)
	}
	return json.Marshal(AnthropicToolChoice{
		Type: "tool",
		Name: obj.Function.Name,
	})
}

// translateAnthropicResponse converts an Anthropic MessagesResponse to an
// OpenAI ChatCompletionResponse. clientModel is echoed back.
func translateAnthropicResponse(resp *MessagesResponse, clientModel string) (*ChatCompletionResponse, error) {
	out := &ChatCompletionResponse{
		ID:      ensurePrefix(resp.ID, "chatcmpl-"),
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   clientModel,
	}

	choice := OpenAIChoice{
		Index:        0,
		FinishReason: mapAnthropicStopReason(resp.StopReason),
	}

	// Separate text and tool_use blocks.
	var texts []string
	var toolCalls []OpenAIToolCall

	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: OpenAIFuncCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "thinking":
			// Dropped.
		}
	}

	if joined := strings.Join(texts, ""); joined != "" {
		choice.Message.Content = json.RawMessage(mustMarshal(joined))
	}
	choice.Message.Role = "assistant"
	choice.Message.ToolCalls = toolCalls

	out.Choices = []OpenAIChoice{choice}

	out.Usage = &OpenAIUsage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}

	return out, nil
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func nowUnix() int64 {
	return time.Now().Unix()
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
