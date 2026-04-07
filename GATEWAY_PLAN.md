# LLM API Gateway — Implementation Plan

Bidirectional translation layer between Anthropic and OpenAI API formats,
embedded in mesh as a new `internal/gateway` package.

## Motivation

Claude Code speaks Anthropic's Messages API. Many LLM backends (Huawei OneAPI,
Ollama, vLLM, Groq, Together, OpenRouter) speak OpenAI's Chat Completions API.
Conversely, many AI tools (Cursor, Aider, Continue, Open WebUI, LibreChat) speak
only OpenAI format but users want to reach Anthropic models.

A bidirectional gateway solves both directions with one component, zero external
dependencies, and no extra processes beyond mesh itself.

## Directions

### Direction A: Anthropic-in, OpenAI-out

Client (e.g. Claude Code) sends Anthropic `/v1/messages` requests.
Gateway translates to OpenAI `/v1/chat/completions` and forwards to the upstream.
Upstream response is translated back to Anthropic format.

Primary use case: Run Claude Code against non-Anthropic backends.

### Direction B: OpenAI-in, Anthropic-out

Client (e.g. Cursor, Aider) sends OpenAI `/v1/chat/completions` requests.
Gateway translates to Anthropic `/v1/messages` and forwards to api.anthropic.com
(or any Anthropic-compatible endpoint).
Upstream response is translated back to OpenAI format.

Primary use case: Use Claude from OpenAI-only tools.

## YAML Configuration

```yaml
my-node:
  gateway:
    - name: claude-to-oneapi
      bind: 127.0.0.1:3457
      # "anthropic-to-openai" or "openai-to-anthropic"
      mode: anthropic-to-openai
      upstream: https://oneapi.rnd.huawei.com/v1/chat/completions
      api_key_env: ONEAPI_KEY           # env var name, never a literal key in config
      proxy: socks5://127.0.0.1:1081    # optional, outbound proxy for upstream
      timeout: 600s                     # upstream request timeout
      model_map:                        # model name remapping
        claude-opus-4-6: glm-4.7
        claude-sonnet-4-6: glm-4.7
      default_max_tokens: 8192         # used when client omits max_tokens (OpenAI allows omission, Anthropic requires it)

    - name: claude-for-cursor
      bind: 127.0.0.1:3458
      mode: openai-to-anthropic
      upstream: https://api.anthropic.com/v1/messages
      api_key_env: ANTHROPIC_API_KEY
      timeout: 600s
      model_map:
        gpt-4o: claude-sonnet-4-6
        gpt-4: claude-sonnet-4-6
      default_max_tokens: 8192
```

Follows existing mesh patterns:
- Array of instances (like `clipsync`, `filesync`)
- `bind` field for local listener
- YAML-driven, validated at startup
- Secrets via env var reference, never in config

## API Surface

### Direction A: Anthropic-in, OpenAI-out

Gateway listens on `bind` and accepts:
- `POST /v1/messages` — main translation endpoint
- `GET /health` — liveness check

### Direction B: OpenAI-in, Anthropic-out

Gateway listens on `bind` and accepts:
- `POST /v1/chat/completions` — main translation endpoint
- `GET /health` — liveness check

## Translation Specification

### A.1 Request: Anthropic -> OpenAI

| Anthropic field | OpenAI field | Notes |
|---|---|---|
| `model` | `model` | Apply `model_map` |
| `max_tokens` | `max_tokens` | Direct |
| `system` (string) | `messages[0]` with `role: "system"` | Prepend to messages array |
| `system` (array of content blocks) | `messages[0]` with `role: "system"` | Concatenate text blocks. Strip `cache_control`. |
| `messages[].role` | `messages[].role` | Direct (`user`/`assistant`) |
| `messages[].content` (string) | `messages[].content` (string) | Direct |
| `messages[].content` (text blocks) | `messages[].content` (string) | Concatenate text blocks |
| `messages[].content` (image block, base64) | `messages[].content` part `image_url` | Convert to `data:{media_type};base64,{data}` URI |
| `messages[].content` (image block, url) | `messages[].content` part `image_url` | Wrap URL |
| `messages[].content` (tool_use block) | `messages[].tool_calls[]` on prior assistant msg | Map: `name`->`function.name`, `input`->`function.arguments` (JSON-stringify), `id`->`id` |
| `messages[].content` (tool_result block) | Message with `role: "tool"` | Map: `tool_use_id`->`tool_call_id`, `content`->`content`. `is_error` has no equivalent — encode in content. |
| `messages[].content` (thinking block) | Drop or pass as system context | No OpenAI equivalent |
| `temperature` | `temperature` | Direct (both use 0-1 range; Anthropic max 1.0, OpenAI max 2.0) |
| `top_p` | `top_p` | Direct |
| `top_k` | Drop | No OpenAI equivalent |
| `stop_sequences` | `stop` | Direct (array) |
| `stream` | `stream` | Direct. Add `stream_options: {include_usage: true}` when true. |
| `tools[]` | `tools[]` | Restructure: Anthropic `input_schema` -> OpenAI `function.parameters`, wrap in `{type: "function", function: {...}}` |
| `tool_choice.type: "auto"` | `tool_choice: "auto"` | |
| `tool_choice.type: "any"` | `tool_choice: "required"` | |
| `tool_choice.type: "tool"` | `tool_choice: {type: "function", function: {name: X}}` | |
| `tool_choice.type: "none"` | `tool_choice: "none"` | |
| `metadata.user_id` | `user` | |

**Dropped fields** (no OpenAI equivalent): `top_k`, `thinking`, `cache_control`.

### A.2 Response: OpenAI -> Anthropic

| OpenAI field | Anthropic field | Notes |
|---|---|---|
| `id` | `id` | Pass through (or prefix `msg_`) |
| — | `type` | Always `"message"` |
| `choices[0].message.role` | `role` | Always `"assistant"` |
| `choices[0].message.content` | `content[0]` | Wrap in `{type: "text", text: ...}` |
| `choices[0].message.tool_calls[]` | `content[]` | Each becomes `{type: "tool_use", id, name, input}`. `input` = parse `function.arguments` from JSON string. |
| `choices[0].finish_reason: "stop"` | `stop_reason: "end_turn"` | |
| `choices[0].finish_reason: "length"` | `stop_reason: "max_tokens"` | |
| `choices[0].finish_reason: "tool_calls"` | `stop_reason: "tool_use"` | |
| `choices[0].finish_reason: "content_filter"` | `stop_reason: "end_turn"` | Best approximation |
| `usage.prompt_tokens` | `usage.input_tokens` | |
| `usage.completion_tokens` | `usage.output_tokens` | |
| `model` | `model` | Reverse `model_map` or pass through |

### A.3 Streaming: OpenAI SSE -> Anthropic SSE

Translation sequence:

1. **First chunk** (contains `delta.role`): Emit `message_start` with skeleton message object.
2. **Text delta chunks** (`delta.content != ""`): 
   - On first text delta: emit `content_block_start` (index 0, type text).
   - Emit `content_block_delta` with `text_delta`.
3. **Tool call chunks** (`delta.tool_calls` present):
   - On first chunk for a tool index (has `id` and `function.name`): emit `content_block_stop` for previous block (if any), then `content_block_start` (type tool_use, with id and name).
   - On argument chunks: emit `content_block_delta` with `input_json_delta`.
4. **Final chunk** (`finish_reason` set): emit `content_block_stop` for last block, then `message_delta` with mapped stop_reason and usage, then `message_stop`.
5. After `data: [DONE]`: close the SSE stream.

Edge cases to handle:
- **Empty `tool_calls: []` in delta**: Ignore. Some backends (mlx_lm.server) always send this. Do NOT enter tool_calls branch.
- **Tool name + arguments in same chunk**: Process both — emit `content_block_start` AND the first `input_json_delta` from the same source chunk.
- **Usage in final chunk vs separate chunk**: OpenAI may send usage in the `finish_reason` chunk or in a separate trailing chunk (when `include_usage: true`). Buffer and emit in `message_delta`.
- **Missing usage**: Some backends don't report usage at all. Emit zeros.

### B.1 Request: OpenAI -> Anthropic

| OpenAI field | Anthropic field | Notes |
|---|---|---|
| `model` | `model` | Apply `model_map` |
| `messages` (system role) | `system` | Extract all system/developer messages, concatenate, set as top-level `system` string |
| `messages` (consecutive same-role) | Merge | Anthropic requires strict user/assistant alternation |
| `messages[].content` (string) | `messages[].content` | Direct |
| `messages[].content` (image_url parts) | Image content blocks | Parse data URI -> base64+media_type, or URL source |
| `messages` (tool role) | `tool_result` content blocks in user message | Map: `tool_call_id`->`tool_use_id`, `content`->`content` |
| `messages` (assistant with tool_calls) | Assistant message with tool_use content blocks | Map: `id`->`id`, `function.name`->`name`, parse `function.arguments`->`input` |
| `max_tokens` / `max_completion_tokens` | `max_tokens` | **Required** in Anthropic. Use `default_max_tokens` from config if omitted. |
| `temperature` | `temperature` | Clamp 0-2 -> 0-1 |
| `top_p` | `top_p` | Direct |
| `stop` | `stop_sequences` | Convert string to array if needed |
| `stream` | `stream` | Direct |
| `tools[]` | `tools[]` | Unwrap: `function.parameters` -> `input_schema`, drop `type: "function"` wrapper |
| `tool_choice: "none"` | `tool_choice: {type: "none"}` | |
| `tool_choice: "auto"` | `tool_choice: {type: "auto"}` | |
| `tool_choice: "required"` | `tool_choice: {type: "any"}` | |
| `tool_choice: {type: "function", function: {name: X}}` | `tool_choice: {type: "tool", name: X}` | |
| `user` | `metadata: {user_id: ...}` | |
| `n` | Drop | Anthropic always returns 1 |
| `frequency_penalty` | Drop | No equivalent |
| `presence_penalty` | Drop | No equivalent |
| `logit_bias` | Drop | No equivalent |
| `logprobs` | Drop | No equivalent |
| `seed` | Drop | No equivalent |
| `response_format` | Drop | No direct equivalent (Anthropic has `output_config` but not widely supported) |

### B.2 Response: Anthropic -> OpenAI

| Anthropic field | OpenAI field | Notes |
|---|---|---|
| `id` | `id` | Pass through (or prefix `chatcmpl-`) |
| — | `object` | `"chat.completion"` |
| — | `created` | `time.Now().Unix()` |
| `model` | `model` | Reverse `model_map` or pass through |
| `content` (text blocks) | `choices[0].message.content` | Concatenate all text block `.text` fields |
| `content` (tool_use blocks) | `choices[0].message.tool_calls[]` | Map: `name`->`function.name`, `input`->`function.arguments` (JSON-stringify), `id`->`id`, add `type: "function"` |
| `content` (thinking blocks) | Drop | No OpenAI equivalent |
| `stop_reason: "end_turn"` | `finish_reason: "stop"` | |
| `stop_reason: "stop_sequence"` | `finish_reason: "stop"` | |
| `stop_reason: "max_tokens"` | `finish_reason: "length"` | |
| `stop_reason: "tool_use"` | `finish_reason: "tool_calls"` | |
| `usage.input_tokens` | `usage.prompt_tokens` | |
| `usage.output_tokens` | `usage.completion_tokens` | |
| — | `usage.total_tokens` | Compute: `prompt_tokens + completion_tokens` |

### B.3 Streaming: Anthropic SSE -> OpenAI SSE

Translation sequence:

1. **`message_start`**: Emit first chunk with `delta: {role: "assistant", content: ""}`, `finish_reason: null`.
2. **`content_block_start` (type text)**: No output needed (OpenAI has no block lifecycle).
3. **`content_block_delta` (text_delta)**: Emit chunk with `delta: {content: "<text>"}`.
4. **`content_block_start` (type tool_use)**: Emit chunk with `delta: {tool_calls: [{index: N, id: "<id>", type: "function", function: {name: "<name>", arguments: ""}}]}`. Track tool index counter.
5. **`content_block_delta` (input_json_delta)**: Emit chunk with `delta: {tool_calls: [{index: N, function: {arguments: "<partial_json>"}}]}`.
6. **`content_block_stop`**: No output needed.
7. **`content_block_start/delta` (thinking)**: Drop silently.
8. **`message_delta`**: Emit chunk with mapped `finish_reason` and empty delta.
9. **`message_stop`**: Emit usage chunk (if client requested `stream_options.include_usage`), then emit `data: [DONE]`.
10. **`ping`**: Ignore.
11. **`error`**: Emit `data: [DONE]` and close (or translate to an error chunk if possible).

### Error Translation

| Anthropic status | OpenAI status | Notes |
|---|---|---|
| 400 `invalid_request_error` | 400 | Direct |
| 401 `authentication_error` | 401 | Direct |
| 402 `billing_error` | 402 | Pass through |
| 403 `permission_error` | 403 | Direct |
| 404 `not_found_error` | 404 | Direct |
| 413 `request_too_large` | 413 | Direct |
| 429 `rate_limit_error` | 429 | Direct |
| 500 `api_error` | 500 | Direct |
| 529 `overloaded_error` | 503 | Anthropic-specific code -> standard 503 |

Error body translation:
- Anthropic: `{"type": "error", "error": {"type": "...", "message": "..."}}`
- OpenAI: `{"error": {"message": "...", "type": "...", "param": null, "code": null}}`

For Direction A (upstream is OpenAI, client expects Anthropic): reverse the mapping.
For Direction B (upstream is Anthropic, client expects OpenAI): use the table as-is.

Mid-stream errors: Anthropic can send `event: error` after HTTP 200. Translate to a
final chunk with an error message in content, then `data: [DONE]`.

## Package Structure

```
internal/gateway/
    config.go       — GatewayCfg struct, validation
    gateway.go      — Start(ctx, cfg) entry point, HTTP listener, route dispatch
    anthropic.go    — Anthropic request/response types
    openai.go       — OpenAI request/response types
    a2o.go          — Anthropic-to-OpenAI: request translation, response translation
    o2a.go          — OpenAI-to-Anthropic: request translation, response translation
    a2o_stream.go   — Anthropic-to-OpenAI streaming SSE translation
    o2a_stream.go   — OpenAI-to-Anthropic streaming SSE translation
    errors.go       — Error type mapping, error response construction
    gateway_test.go — Round-trip translation tests for both directions
    stream_test.go  — Streaming translation tests
```

## Implementation Sequence

### Phase 1: Types and non-streaming translation

1. **config.go**: Define `GatewayCfg` struct. Add `Gateway []GatewayCfg` to
   `config.Config`. Add validation in `config.Validate()`.
2. **anthropic.go**: Define all Anthropic request/response types: `MessagesRequest`,
   `MessagesResponse`, `ContentBlock`, `ToolDefinition`, `Usage`, `ErrorResponse`, etc.
3. **openai.go**: Define all OpenAI request/response types: `ChatCompletionRequest`,
   `ChatCompletionResponse`, `ChatCompletionMessage`, `ToolCall`, `Tool`,
   `Usage`, `ErrorResponse`, etc.
4. **a2o.go**: Implement `translateAnthropicRequest(req *MessagesRequest, cfg *GatewayCfg) (*ChatCompletionRequest, error)` and
   `translateOpenAIResponse(resp *ChatCompletionResponse, cfg *GatewayCfg) (*MessagesResponse, error)`.
5. **o2a.go**: Implement `translateOpenAIRequest(req *ChatCompletionRequest, cfg *GatewayCfg) (*MessagesRequest, error)` and
   `translateAnthropicResponse(resp *MessagesResponse, cfg *GatewayCfg) (*ChatCompletionResponse, error)`.
6. **gateway_test.go**: Table-driven tests covering:
   - Simple text message round-trip
   - System message extraction/injection
   - Tool definitions translation
   - Tool use + tool result conversation flow
   - Image content blocks
   - Model name mapping
   - All finish_reason/stop_reason mappings
   - Consecutive same-role message merging (Direction B)
   - Missing max_tokens default injection
   - Temperature clamping
7. **errors.go**: Error type mapping and response construction for both directions.

### Phase 2: HTTP handler and upstream forwarding

8. **gateway.go**: Implement `Start(ctx context.Context, cfg GatewayCfg, log *slog.Logger) error`:
   - Listen on `cfg.Bind`
   - Route `POST /v1/messages` (Direction A) or `POST /v1/chat/completions` (Direction B)
   - Route `GET /health` -> 200 OK
   - For each request: translate -> forward to upstream -> translate response -> return
   - Use `http.Client` with optional proxy (`cfg.Proxy`) and configurable timeout
   - Register in `state.Global` for dashboard visibility
   - Log request/response metadata (model, tokens, latency) via slog
9. **cmd/mesh/main.go**: Wire gateway into `upCmd()` alongside other components.

### Phase 3: Streaming

10. **a2o_stream.go**: Direction A streaming — read OpenAI SSE from upstream, translate
    each chunk to Anthropic SSE events, flush to client.
11. **o2a_stream.go**: Direction B streaming — read Anthropic SSE from upstream, translate
    each event to OpenAI SSE chunks, flush to client.
12. **stream_test.go**: Tests with recorded SSE fixtures:
    - Plain text streaming
    - Tool call streaming (single and parallel)
    - Empty tool_calls array handling
    - Tool name + arguments in same chunk
    - Missing usage in stream
    - Mid-stream error handling

### Phase 4: Integration and hardening

13. Register gateway metrics in `state.Global` (requests, bytes, latency, errors).
14. Add `gateway` section to `configs/example.yaml` and update JSON schema.
15. Test end-to-end: Claude Code -> gateway -> Ollama local model.

## Known Edge Cases to Handle

From field reports across LiteLLM, one-api, vLLM, and other gateways:

1. **Empty `tool_calls: []` in OpenAI delta** — some backends (mlx_lm.server) always
   populate this even for text responses. Check `len > 0`, not just `!= nil`.

2. **Tool name + arguments in same OpenAI chunk** — process both: emit
   `content_block_start` AND `input_json_delta` from the same source event.

3. **Empty tool arguments** — streaming may send `arguments: ""` for zero-arg tools.
   Normalize to `{}` when parsing to Anthropic `input` object.

4. **Image media_type from data URI** — when converting OpenAI `data:image/png;base64,...`
   to Anthropic format, extract `media_type` from the URI prefix. Supported types:
   `image/jpeg`, `image/png`, `image/gif`, `image/webp`.

5. **Consecutive same-role messages** — OpenAI allows multiple `user` messages in a row.
   Anthropic requires strict alternation. Merge adjacent same-role messages by
   concatenating content with `\n\n`.

6. **Multiple system messages** — OpenAI allows system messages interspersed anywhere.
   Anthropic requires a single top-level `system`. Concatenate all system messages.

7. **Anthropic `max_tokens` is required** — OpenAI allows omission. Use
   `default_max_tokens` from config as fallback.

8. **Usage reporting** — some OpenAI-compatible backends don't report usage at all.
   Return zeros rather than failing.

9. **Anthropic 529 overloaded** — non-standard HTTP status. Translate to 503 for
   OpenAI clients.

10. **SSE mid-stream errors** — Anthropic can send `event: error` after HTTP 200.
    OpenAI has no equivalent. Translate to a final text chunk with the error message,
    then `data: [DONE]`.

11. **`tool_result` content can be complex** — Anthropic allows nested content blocks
    (text + image) inside `tool_result`. OpenAI `tool` role expects a string. When
    going Anthropic->OpenAI, JSON-encode complex content. When going OpenAI->Anthropic,
    pass string directly.

12. **`parallel_tool_calls: false`** — OpenAI parameter. Anthropic has no equivalent.
    Drop when translating to Anthropic (Claude decides independently).

13. **`is_error` on tool results** — Anthropic supports `is_error: true` on
    `tool_result` blocks. OpenAI has no equivalent. When going OpenAI->Anthropic,
    set `is_error: false`. When going Anthropic->OpenAI, prepend `[ERROR] ` to
    content string if `is_error: true`.

14. **Temperature range** — Anthropic: 0.0-1.0. OpenAI: 0.0-2.0. When translating
    OpenAI->Anthropic, clamp values > 1.0 to 1.0.

## Non-Goals (Explicitly Out of Scope)

The guiding principle: translate faithfully what can be translated, drop silently
what can't, don't build infrastructure for problems we don't have.

- **Batch API** — Anthropic's `POST /v1/messages/batches` endpoint submits many
  requests for async processing. Claude Code and similar tools make interactive,
  real-time requests — batch adds complexity for a use case we don't have.

- **Token counting endpoint** — `POST /v1/messages/count_tokens`. Low priority.
  Could be added later if needed.

- **Computer use tools** — Anthropic-specific tool types (bash, editor, computer).
  These are Claude-specific capabilities that don't translate to other models.

- **Prompt caching** — `cache_control` fields tell Anthropic's servers to cache
  parts of the prompt for cheaper repeated calls. This is a server-side feature —
  it only works when the upstream is actually Anthropic. Passing `cache_control`
  to an OpenAI backend does nothing, so we strip it silently rather than pretending
  it works.

- **Extended thinking** — `thinking` parameter and thinking blocks. No useful
  mapping to OpenAI's `reasoning_effort` (which provides no visibility into
  reasoning). Stripped silently.

- **Embeddings API** — Anthropic has none. OpenAI has `POST /v1/embeddings` for
  vector representations. Different problem domain entirely — not what a chat
  translation gateway is for.

- **Audio/modalities** — OpenAI-specific. Stripped silently.

- **`n > 1`** — Multiple completions. Anthropic always returns exactly 1.
  Log a warning, use 1.

- **`response_format`** — JSON mode / structured outputs. No clean bidirectional
  mapping. Stripped.

- **Multi-provider routing** — The gateway translates formats, it does not route
  between backends. Each gateway instance points to one upstream. If you want two
  backends (e.g., OneAPI + Ollama), configure two gateway entries in YAML with
  different `bind` ports. Building a router with fallback logic, model-based
  routing, and load balancing is what LiteLLM does with 42k stars and a
  Python+Redis+PostgreSQL stack — the opposite of what we want.

- **API key management / rotation** — Single key per instance. Use mesh config to
  run multiple instances if needed.

## Client Configuration Examples

### Claude Code -> Huawei OneAPI via mesh gateway

```json
// ~/.claude/settings.json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:3457"
  }
}
```

Then use `/model` to select any model name — the gateway maps it via `model_map`.

### Cursor -> Claude via mesh gateway

In Cursor settings, add a custom OpenAI-compatible endpoint:
- Base URL: `http://127.0.0.1:3458/v1`
- API key: any non-empty string (gateway handles the real key)
- Model: `gpt-4o` (mapped to `claude-sonnet-4-6` by the gateway)

### Aider -> Claude via mesh gateway

```bash
export OPENAI_API_BASE=http://127.0.0.1:3458/v1
export OPENAI_API_KEY=dummy
aider --model gpt-4o
```
