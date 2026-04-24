package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
)

// defaultKeepRecent is the number of trailing messages preserved verbatim
// when summarizing. 6 messages ≈ 3 turn pairs (user+assistant), giving the
// model its immediate working context.
const defaultKeepRecent = 6

// bytesPerToken is the whole-body divisor used by estimateTokens. Calibrated
// against cl100k_base tokenization of testdata/calibration_request.json such
// that the estimator lands inside [0.95×, 1.25×] of the actual token count
// (see summarize_calibration_test.go). The bias is deliberately high — if
// the estimator is off, we prefer over-counting (admission-control error
// fails closed: summarize or reject earlier, never silently exceed the
// upstream's window).
//
// cl100k_base is a proxy. Anthropic does not publish their tokenizer, so
// we have no ground truth for Claude bodies. cl100k lands within single-
// digit percent of Claude tokenization on typical English+code mixes;
// good enough for admission control, not good enough for billing. Phase
// 1a audit rows will carry actual Claude usage counts in the response,
// and Phase 1b will use those to validate or re-tune this constant.
//
// Calibration bias caveat: testdata/calibration_request.json is
// lorem-ipsum-heavy; lorem tokenizes unusually well (long Latin words,
// low vocabulary churn, no structural noise). Real Claude Code bodies
// are mostly JSON scaffolding + English prose + source code + shell
// output, which tokenizes worse (shorter tokens, more punctuation,
// more symbols). The 1.19 ratio we hit on the fixture is likely
// 1.05–1.10 on real traffic — still inside the floor, but closer to
// it. Phase 1b revisit: sample real `usage.input_tokens` from Phase
// 1a audit rows and recheck the ratio. If it drops below ~1.05, tune
// this constant down (not the envelope).
const bytesPerToken = 4.5

// estimateTokens returns a rough token count for an Anthropic messages
// request from its wire-JSON body. Counting whole-body bytes (not
// piece-wise) captures tools, tool_choice, metadata, stop_sequences,
// system (string or block-array form), and any other scaffolding the
// upstream tokenizer sees — all without maintaining a parallel sum that
// drifts when request shape changes.
func estimateTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	return int(math.Ceil(float64(len(body)) / bytesPerToken))
}

// serializeMessages converts Anthropic messages to a plain-text dump suitable
// for a summarizer. Each message is prefixed with its role. Content blocks are
// rendered as text; images become [image], tool_use becomes [tool: name(...)],
// tool_result becomes [result: ...], and thinking blocks are dropped.
func serializeMessages(msgs []AnthropicMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(strings.ToUpper(m.Role))
		b.WriteString(":\n")

		// Content can be a plain string or an array of ContentBlock.
		var text string
		if len(m.Content) > 0 && m.Content[0] == '"' {
			_ = json.Unmarshal(m.Content, &text)
			b.WriteString(text)
			b.WriteByte('\n')
		} else {
			var blocks []ContentBlock
			if err := json.Unmarshal(m.Content, &blocks); err == nil {
				for _, bl := range blocks {
					switch bl.Type {
					case "text":
						b.WriteString(bl.Text)
						b.WriteByte('\n')
					case "image":
						b.WriteString("[image]\n")
					case "tool_use":
						_, _ = fmt.Fprintf(&b, "[tool: %s(%s)]\n", bl.Name, truncateStr(string(bl.Input), 200))
					case "tool_result":
						var resultText string
						if len(bl.Content) > 0 && bl.Content[0] == '"' {
							_ = json.Unmarshal(bl.Content, &resultText)
						} else {
							resultText = string(bl.Content)
						}
						_, _ = fmt.Fprintf(&b, "[result: %s]\n", truncateStr(resultText, 500))
					case "thinking":
						// Drop thinking blocks — they're internal reasoning.
					default:
						_, _ = fmt.Fprintf(&b, "[%s]\n", bl.Type)
					}
				}
			} else {
				// Unparseable — include raw (truncated).
				b.WriteString(truncateStr(string(m.Content), 500))
				b.WriteByte('\n')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

const summarizerSystemPrompt = `You are a conversation summarizer for an AI coding assistant session. Condense the conversation history into a concise summary that preserves:
1. Key decisions and their reasoning
2. Current state of any ongoing tasks
3. File paths and code changes mentioned
4. Constraints, requirements, or user preferences established
5. Tool calls made and their outcomes

Be factual and specific. Output only the summary, no preamble.`

// summarizeMessages calls the summarizer upstream to condense old messages.
// It preserves the most recent keepRecent messages verbatim and summarizes
// everything before them. Returns a replacement message slice.
func summarizeMessages(
	ctx context.Context,
	req *MessagesRequest,
	summarizer *ResolvedUpstream,
	keepRecent int,
	log *slog.Logger,
) ([]AnthropicMsg, error) {
	if keepRecent <= 0 {
		keepRecent = defaultKeepRecent
	}
	if len(req.Messages) <= keepRecent {
		return req.Messages, nil
	}

	desiredCutoff := len(req.Messages) - keepRecent
	cutoff := safeCutoff(req.Messages, desiredCutoff)
	if cutoff == 0 {
		// No safe cut exists — every candidate boundary would split a
		// tool_use from its tool_result. Summarizing would produce a
		// request Anthropic rejects with 400 "tool_result without
		// matching tool_use". Skip summarization; caller will retry or
		// fail with a clear context-exceeded error.
		log.Warn("summarizer skip: no tool-safe cutoff", "messages", len(req.Messages), "keep_recent", keepRecent)
		return req.Messages, nil
	}
	if cutoff != desiredCutoff {
		log.Info("summarizer cutoff moved for tool-boundary safety",
			"desired_cutoff", desiredCutoff,
			"safe_cutoff", cutoff,
			"messages_extended", desiredCutoff-cutoff,
		)
	}
	toSummarize := req.Messages[:cutoff]
	recent := req.Messages[cutoff:]

	dump := serializeMessages(toSummarize)
	if len(dump) == 0 {
		return req.Messages, nil
	}

	// Build an OpenAI chat completion request for the summarizer.
	maxTok := 4096
	oaiReq := ChatCompletionRequest{
		Model: summarizer.Cfg.MapModel("summarizer"),
		Messages: []OpenAIMsg{
			{Role: "system", Content: json.RawMessage(mustMarshalString(summarizerSystemPrompt))},
			{Role: "user", Content: json.RawMessage(mustMarshalString(dump))},
		},
		MaxTokens: &maxTok,
	}
	oaiBody, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal summarizer request: %w", err)
	}

	headers := map[string]string{}
	if summarizer.APIKey != "" {
		headers["Authorization"] = "Bearer " + summarizer.APIKey
	}

	statusCode, respBody, err := doUpstreamRequest(ctx, summarizer.Client, summarizer.Cfg.Target, oaiBody, headers, log)
	if err != nil {
		return nil, fmt.Errorf("summarizer request failed: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("summarizer returned status %d: %s", statusCode, truncateStr(string(respBody), 500))
	}

	var oaiResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("parse summarizer response: %w", err)
	}
	if len(oaiResp.Choices) == 0 {
		return nil, fmt.Errorf("summarizer returned no choices")
	}

	var summaryText string
	_ = json.Unmarshal(oaiResp.Choices[0].Message.Content, &summaryText)
	if summaryText == "" {
		summaryText = string(oaiResp.Choices[0].Message.Content)
	}

	summaryContent := fmt.Sprintf("[Conversation summary — %d messages condensed]\n\n%s\n\n[End of summary. The conversation continues below.]",
		len(toSummarize), summaryText)

	summaryMsg := AnthropicMsg{
		Role:    "user",
		Content: json.RawMessage(mustMarshalString(summaryContent)),
	}

	// The first recent message must be a user message (Anthropic requires
	// alternating roles starting with user). If the summary (user) is followed
	// by another user message, insert a minimal assistant acknowledgement.
	result := []AnthropicMsg{summaryMsg}
	if len(recent) > 0 && recent[0].Role == "user" {
		result = append(result, AnthropicMsg{
			Role:    "assistant",
			Content: json.RawMessage(mustMarshalString("Understood. I have the context from the summary above.")),
		})
	}
	result = append(result, recent...)

	log.Info("Conversation summarized",
		"messages_before", len(req.Messages),
		"messages_summarized", len(toSummarize),
		"messages_after", len(result),
		"summary_tokens", len(summaryText)/4,
	)
	return result, nil
}

// contextCheckResult describes what happened when checking context limits.
type contextCheckResult int

const (
	contextOK         contextCheckResult = iota // within limits or no limit configured
	contextExceeded                             // exceeded and no summarizer configured
	contextSummarized                           // exceeded and successfully summarized
	contextError                                // exceeded but summarization failed
)

// contextCheckInfo captures the estimated token counts seen during context
// enforcement so audit rows can explain what happened.
type contextCheckInfo struct {
	OriginalTokens  int
	EffectiveTokens int
	Summarized      bool
}

// checkAndSummarize checks if body exceeds the upstream's context window.
// If it does and a summarizer is configured, it summarizes the message history.
// Returns the (possibly modified) body, the result code, token estimate info,
// and any error.
func checkAndSummarize(
	ctx context.Context,
	body []byte,
	upstream *ResolvedUpstream,
	router *Router,
	log *slog.Logger,
) ([]byte, contextCheckResult, contextCheckInfo, error) {
	if !upstream.Cfg.HasContextLimit() {
		return body, contextOK, contextCheckInfo{}, nil
	}

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return body, contextOK, contextCheckInfo{}, nil // can't parse — let the handler deal with it
	}

	estimated := estimateTokens(body)
	info := contextCheckInfo{OriginalTokens: estimated, EffectiveTokens: estimated}
	if estimated <= upstream.Cfg.ContextWindow {
		return body, contextOK, info, nil
	}

	log.Info("Request exceeds context window",
		"estimated_tokens", estimated,
		"context_window", upstream.Cfg.ContextWindow,
		"messages", len(req.Messages),
	)

	if upstream.Cfg.Summarizer == "" {
		return body, contextExceeded, info, fmt.Errorf(
			"input context too large: estimated %d tokens exceeds upstream context window of %d tokens (default_max_tokens only limits output tokens)",
			estimated, upstream.Cfg.ContextWindow)
	}

	sumUpstream := router.Upstream(upstream.Cfg.Summarizer)
	if sumUpstream == nil {
		return body, contextError, info, fmt.Errorf(
			"summarizer upstream %q not found", upstream.Cfg.Summarizer)
	}

	newMsgs, err := summarizeMessages(ctx, &req, sumUpstream, defaultKeepRecent, log)
	if err != nil {
		return body, contextError, info, fmt.Errorf("local summarization failed before forwarding upstream: %w", err)
	}

	req.Messages = newMsgs
	newBody, err := json.Marshal(req)
	if err != nil {
		return body, contextError, info, fmt.Errorf("re-serialize failed: %w", err)
	}
	newEstimated := estimateTokens(newBody)
	info.EffectiveTokens = newEstimated
	info.Summarized = true
	if newEstimated > upstream.Cfg.ContextWindow {
		return body, contextExceeded, info, fmt.Errorf(
			"input context still too large after local summarization: %d tokens exceeds upstream context window of %d tokens; start a fresh session or reduce conversation history",
			newEstimated, upstream.Cfg.ContextWindow)
	}

	return newBody, contextSummarized, info, nil
}

func mustMarshalString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// extractToolUseIDs returns the IDs of every tool_use content block in m.
// Returns nil for string-content or non-block-array messages.
func extractToolUseIDs(m AnthropicMsg) []string {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// extractToolResultIDs returns the tool_use IDs referenced by every
// tool_result content block in m.
func extractToolResultIDs(m AnthropicMsg) []string {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			ids = append(ids, b.ToolUseID)
		}
	}
	return ids
}

// safeCutoff returns the largest cutoff ≤ desired such that no tool_result
// in messages[cutoff:] references a tool_use in messages[:cutoff]. This
// prevents summarization from splitting a tool_use/tool_result pair, which
// Anthropic rejects with "tool_result without matching tool_use".
//
// Returns 0 when no safe cut exists — callers must treat this as "do not
// summarize" rather than "summarize everything".
//
// Complexity: O(n²) worst case. n is the conversation length, typically
// under 100; real cost is negligible.
func safeCutoff(messages []AnthropicMsg, desired int) int {
	if desired <= 0 {
		return 0
	}
	if desired >= len(messages) {
		return len(messages)
	}
	for cutoff := desired; cutoff > 0; cutoff-- {
		prefixIDs := make(map[string]struct{})
		for _, m := range messages[:cutoff] {
			for _, id := range extractToolUseIDs(m) {
				prefixIDs[id] = struct{}{}
			}
		}
		safe := true
		for _, m := range messages[cutoff:] {
			for _, id := range extractToolResultIDs(m) {
				if _, inPrefix := prefixIDs[id]; inPrefix {
					safe = false
					break
				}
			}
			if !safe {
				break
			}
		}
		if safe {
			return cutoff
		}
	}
	return 0
}
