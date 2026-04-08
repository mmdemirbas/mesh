package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// translateAnthropicRequest converts an Anthropic MessagesRequest to an OpenAI
// ChatCompletionRequest. The original client model name is returned so the
// response can echo it back unchanged.
func translateAnthropicRequest(req *MessagesRequest, cfg *GatewayCfg) (*ChatCompletionRequest, error) {
	out := &ChatCompletionRequest{
		Model:    cfg.MapModel(req.Model),
		Stream:   req.Stream,
	}

	if req.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = cfg.MaxTokens()
	}
	out.MaxTokens = &maxTok

	if req.Temperature != nil {
		out.Temperature = req.Temperature
	}
	if req.TopP != nil {
		out.TopP = req.TopP
	}
	// top_k: dropped (no OpenAI equivalent)

	if len(req.StopSequences) > 0 {
		b, _ := json.Marshal(req.StopSequences)
		out.Stop = b
	}

	if req.Metadata != nil && req.Metadata.UserID != "" {
		out.User = req.Metadata.UserID
	}

	// System message
	if len(req.System) > 0 {
		sysText, err := extractSystemText(req.System)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if sysText != "" {
			sysContent, _ := json.Marshal(sysText)
			out.Messages = append(out.Messages, OpenAIMsg{
				Role:    "system",
				Content: sysContent,
			})
		}
	}

	// Messages
	msgs, err := translateAnthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	out.Messages = append(out.Messages, msgs...)

	// Tools
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			strict := false
			out.Tools = append(out.Tools, OpenAITool{
				Type: "function",
				Function: OpenAIFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
					Strict:      &strict,
				},
			})
		}
	}

	// Tool choice
	if len(req.ToolChoice) > 0 {
		tc, err := translateAnthropicToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// extractSystemText handles both string and array forms of the Anthropic system field.
func extractSystemText(raw json.RawMessage) (string, error) {
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try array of content blocks.
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("cannot parse system field: %w", err)
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// translateAnthropicMessages converts Anthropic messages to OpenAI messages.
func translateAnthropicMessages(msgs []AnthropicMsg) ([]OpenAIMsg, error) {
	var out []OpenAIMsg

	for _, m := range msgs {
		converted, err := translateAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}

	return out, nil
}

// translateAnthropicMessage converts a single Anthropic message to one or more OpenAI messages.
func translateAnthropicMessage(m AnthropicMsg) ([]OpenAIMsg, error) {
	// Content can be a plain string.
	var contentStr string
	if err := json.Unmarshal(m.Content, &contentStr); err == nil {
		c, _ := json.Marshal(contentStr)
		return []OpenAIMsg{{Role: m.Role, Content: c}}, nil
	}

	// Content is an array of blocks.
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("cannot parse message content: %w", err)
	}

	// Separate blocks by type.
	var textParts []ContentPart
	var toolCalls []OpenAIToolCall
	var toolResults []OpenAIMsg
	var imageFollowUps []OpenAIMsg // images from tool_results

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, ContentPart{
				Type: "text",
				Text: b.Text,
			})

		case "image":
			if b.Source != nil {
				part := ContentPart{Type: "image_url"}
				if b.Source.Type == "base64" {
					part.ImageURL = &ImageURL{
						URL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
					}
				} else {
					part.ImageURL = &ImageURL{URL: b.Source.URL}
				}
				textParts = append(textParts, part)
			}

		case "tool_use":
			args, _ := json.Marshal(b.Input)
			// Normalize empty/null input to "{}".
			if len(args) == 0 || string(args) == "null" {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: OpenAIFuncCall{
					Name:      b.Name,
					Arguments: string(args),
				},
			})

		case "tool_result":
			content, imgs := extractToolResultParts(b)
			if b.IsError {
				content = "[ERROR] " + content
			}
			c, _ := json.Marshal(content)
			toolResults = append(toolResults, OpenAIMsg{
				Role:       "tool",
				Content:    c,
				ToolCallID: b.ToolUseID,
			})
			for _, img := range imgs {
				imgContent, _ := json.Marshal([]ContentPart{
					{Type: "text", Text: "[tool result image for tool_use_id=" + b.ToolUseID + "]"},
					img,
				})
				imageFollowUps = append(imageFollowUps, OpenAIMsg{
					Role:    "user",
					Content: imgContent,
				})
			}

		case "thinking":
			// Dropped — no OpenAI equivalent.
		}
	}

	var out []OpenAIMsg

	// Assistant message with text and/or tool calls.
	if m.Role == "assistant" {
		msg := OpenAIMsg{Role: "assistant"}
		if len(textParts) > 0 {
			// If only text parts, concatenate to string.
			if allText(textParts) {
				text := concatenateText(textParts)
				c, _ := json.Marshal(text)
				msg.Content = c
			} else {
				c, _ := json.Marshal(textParts)
				msg.Content = c
			}
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		out = append(out, msg)
	} else if m.Role == "user" {
		// User message may have text, images, and tool_results.
		// Tool results become separate tool-role messages.
		if len(textParts) > 0 {
			if allText(textParts) {
				text := concatenateText(textParts)
				c, _ := json.Marshal(text)
				out = append(out, OpenAIMsg{Role: "user", Content: c})
			} else {
				c, _ := json.Marshal(textParts)
				out = append(out, OpenAIMsg{Role: "user", Content: c})
			}
		}
		out = append(out, toolResults...)
		out = append(out, imageFollowUps...)
	}

	return out, nil
}

// extractToolResultParts extracts text and images from a tool_result content
// field in a single unmarshal pass.
func extractToolResultParts(b ContentBlock) (string, []ContentPart) {
	if len(b.Content) == 0 {
		return "", nil
	}

	// Try string.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s, nil
	}

	// Try array of blocks — extract text and images in one pass.
	var blocks []ContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		return string(b.Content), nil
	}

	var textParts []string
	var imgs []ContentPart
	for _, sub := range blocks {
		switch sub.Type {
		case "text":
			if sub.Text != "" {
				textParts = append(textParts, sub.Text)
			}
		case "image":
			if sub.Source != nil {
				part := ContentPart{Type: "image_url"}
				if sub.Source.Type == "base64" {
					part.ImageURL = &ImageURL{
						URL: "data:" + sub.Source.MediaType + ";base64," + sub.Source.Data,
					}
				} else {
					part.ImageURL = &ImageURL{URL: sub.Source.URL}
				}
				imgs = append(imgs, part)
			}
		}
	}
	return strings.Join(textParts, "\n\n"), imgs
}

func allText(parts []ContentPart) bool {
	for _, p := range parts {
		if p.Type != "text" {
			return false
		}
	}
	return true
}

func concatenateText(parts []ContentPart) string {
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// translateAnthropicToolChoice converts Anthropic tool_choice to OpenAI format.
func translateAnthropicToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	var tc AnthropicToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("cannot parse tool_choice: %w", err)
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(OpenAIToolChoiceFunc{
			Type:     "function",
			Function: OpenAIToolChoiceFuncName{Name: tc.Name},
		})
	default:
		return json.Marshal("auto")
	}
}

// translateOpenAIResponse converts an OpenAI ChatCompletionResponse to an
// Anthropic MessagesResponse. clientModel is the model name the client
// originally sent — echoed back unchanged.
func translateOpenAIResponse(resp *ChatCompletionResponse, clientModel string) (*MessagesResponse, error) {
	out := &MessagesResponse{
		ID:    ensurePrefix(resp.ID, "msg_"),
		Type:  "message",
		Role:  "assistant",
		Model: clientModel,
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]

		// Map finish_reason -> stop_reason.
		out.StopReason = mapOpenAIFinishReason(choice.FinishReason)

		// Text content.
		if len(choice.Message.Content) > 0 {
			var text string
			if err := json.Unmarshal(choice.Message.Content, &text); err == nil && text != "" {
				out.Content = append(out.Content, ContentBlock{
					Type: "text",
					Text: text,
				})
			}
		}

		// Tool calls.
		for _, tc := range choice.Message.ToolCalls {
			var input json.RawMessage
			if tc.Function.Arguments != "" {
				input = json.RawMessage(tc.Function.Arguments)
			} else {
				input = json.RawMessage("{}")
			}
			out.Content = append(out.Content, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	// Ensure content is never nil.
	if out.Content == nil {
		out.Content = []ContentBlock{}
	}

	// Usage.
	if resp.Usage != nil {
		out.Usage = AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return out, nil
}

func mapOpenAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func ensurePrefix(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		return s
	}
	return prefix + s
}
