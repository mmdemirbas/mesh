package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Workstream A.4 — non-streaming dispatch with key rotation.
//
// dispatchWithKeyRotation wraps doUpstreamRequestFull with a loop
// that picks a key from the upstream's pool, dispatches, and on
// rate-limit (429) tries another key — up to len(pool) attempts.
// Other 5xx / network failures are surfaced to the caller without
// retry within the same upstream; the chain dispatcher (A.5) will
// advance to the next upstream when chains land.
//
// For passthrough upstreams (zero-key pools), the wrapper makes a
// single attempt with empty Authorization (the caller-supplied
// extraHeaders carry the client's auth verbatim, per the
// established passthrough convention).
//
// Single-key upstreams short-circuit to one attempt — there is
// nothing to rotate to.
//
// See DESIGN_WORKSTREAM_A.local.md §3.1, §4.

// dispatchResult carries the outcome of one dispatchWithKeyRotation
// call back to the handler. Status / body / headers are the final
// attempt's response (the one the handler will translate or pass
// through to the client). Attempts is the per-attempt audit history
// A.6 stamps onto the audit row.
type dispatchResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Err        error
	Attempts   []Attempt
}

// dispatchOpts configures one dispatchWithKeyRotation call.
type dispatchOpts struct {
	UpstreamName string
	SessionID    string // for sticky_session policy + audit
	ExtraHeaders map[string]string
	// MaxAttempts caps total attempts within the wrapper. Zero =
	// len(pool) for multi-key, 1 for single/passthrough. Bounded
	// at 8 regardless of pool size to defend against
	// pathologically-large pools or systemic 429 storms.
	MaxAttempts int
}

const dispatchAttemptsCap = 8

// dispatchWithKeyRotation is the non-streaming dispatch helper. It
// picks a key, sends the request, classifies the outcome, and on
// rate-limit retries with another key.
func dispatchWithKeyRotation(ctx context.Context, up *ResolvedUpstream, body []byte, opts dispatchOpts, log *slog.Logger) dispatchResult {
	if up == nil {
		return dispatchResult{Err: errNilUpstream}
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = up.Keys.Len()
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
	}
	if maxAttempts > dispatchAttemptsCap {
		maxAttempts = dispatchAttemptsCap
	}

	var attempts []Attempt
	var lastResult dispatchResult
	for try := 0; try < maxAttempts; try++ {
		rc := RequestContext{Now: time.Now(), SessionID: opts.SessionID, AttemptIndex: try}
		key := up.Keys.Pick(ctx, rc)
		// Build per-request headers. Start from the caller's extras,
		// then layer the chosen key's auth header on top. Empty key
		// (passthrough) leaves the caller's Authorization in place.
		headers := make(map[string]string, len(opts.ExtraHeaders)+1)
		for k, v := range opts.ExtraHeaders {
			headers[k] = v
		}
		var keyID string
		if key != nil {
			keyID = key.ID
			if key.Value != "" {
				applyAuthHeaders(headers, up.Cfg.API, key.Value)
			}
		}

		startedAt := time.Now()
		status, respHeaders, respBody, err := doUpstreamRequestFull(ctx, up.Client, up.Cfg.Target, body, headers, log)
		endedAt := time.Now()
		outcome := classifyOutcome(status, err)

		// Update key state.
		if key != nil {
			if outcome == AttemptRateLimited {
				retryAfter, _ := parseRetryAfter(respHeaders, respBody, up.Cfg.API, endedAt)
				key.MarkDegraded(endedAt.Add(retryAfter))
			}
			recordPassiveOutcome(key, up.Cfg.Health, outcome, endedAt)
		}

		att := Attempt{
			UpstreamName: opts.UpstreamName,
			KeyID:        keyID,
			StartedAt:    startedAt,
			EndedAt:      endedAt,
			Outcome:      outcome,
			StatusCode:   status,
		}
		if err != nil {
			att.Error = err.Error()
		}
		attempts = append(attempts, att)
		lastResult = dispatchResult{
			StatusCode: status,
			Headers:    respHeaders,
			Body:       respBody,
			Err:        err,
		}

		// Decide whether to retry within this upstream. Only
		// rate-limited (429) attempts trigger a retry — 5xx /
		// network / timeout will fall through to A.5's chain
		// advance once chains land. 4xx other than 429 is a
		// request shape problem and never retries.
		if outcome != AttemptRateLimited {
			break
		}
		// Need another usable key to keep going. If all keys are
		// degraded the next Pick returns nil; bail out and let the
		// caller surface the last 429.
		if !up.Keys.AnyUsable(time.Now()) {
			attempts[len(attempts)-1].FallbackReason = "all_keys_rate_limited"
			break
		}
		attempts[len(attempts)-1].FallbackReason = "rate_limited_rotate_key"
	}

	lastResult.Attempts = attempts
	return lastResult
}

// errNilUpstream guards the no-upstream input case. Production
// callers never hit it (handler resolves the upstream before
// calling dispatch), but defensive return keeps the test surface
// clean.
var errNilUpstream = newDispatchError("nil upstream")

type dispatchError string

func (e dispatchError) Error() string           { return string(e) }
func newDispatchError(msg string) dispatchError { return dispatchError(msg) }

// recordStreamAttempt mirrors dispatchWithKeyRotation's per-attempt
// bookkeeping for the streaming path. The streaming handlers cannot
// reuse dispatchWithKeyRotation directly because that helper buffers
// the entire response body (incompatible with SSE), but the side-
// effects on the picked key and the audit row's Attempts slice need
// to match. This was REVIEW B1/I1: pre-fix the streaming path never
// updated key health and produced audit rows with no upstream/key
// attribution.
//
// Call once per upstream interaction:
//   - On request-level failure (transport error before any response):
//     status=0, headers/body nil, err non-nil.
//   - On non-200 response (incl. 429 / 5xx): pass status, response
//     headers, response body, err=nil.
//   - On scanner-level error mid-stream after a 200: status=200, err
//     non-nil (treated as AttemptNetworkError).
//   - On clean stream completion: status=200, err=nil.
//
// respHeaders / respBody are only consulted for 429 → Retry-After
// parsing; they may be nil for other paths.
func recordStreamAttempt(au *AuditUpstream, upstream *ResolvedUpstream, key *KeyState, start time.Time, status int, respHeaders http.Header, respBody []byte, err error) {
	end := time.Now()
	outcome := classifyOutcome(status, err)
	if key != nil {
		if outcome == AttemptRateLimited {
			retryAfter, _ := parseRetryAfter(respHeaders, respBody, upstream.Cfg.API, end)
			key.MarkDegraded(end.Add(retryAfter))
		}
		recordPassiveOutcome(key, upstream.Cfg.Health, outcome, end)
	}
	if au == nil {
		return
	}
	att := Attempt{
		UpstreamName: upstream.Cfg.Name,
		StartedAt:    start,
		EndedAt:      end,
		Outcome:      outcome,
		StatusCode:   status,
	}
	if key != nil {
		att.KeyID = key.ID
	}
	if err != nil {
		att.Error = err.Error()
	}
	au.Attempts = append(au.Attempts, att)
}

// applyAuthHeaders writes the upstream's expected auth header(s)
// for the given API shape. Anthropic uses x-api-key + a required
// anthropic-version header; OpenAI uses Authorization: Bearer.
// Unknown API shapes default to Authorization: Bearer (the
// dominant convention).
func applyAuthHeaders(h map[string]string, api, key string) {
	switch api {
	case APIAnthropic:
		h["x-api-key"] = key
		h["anthropic-version"] = "2023-06-01"
	case APIOpenAI:
		h["Authorization"] = "Bearer " + key
	default:
		h["Authorization"] = "Bearer " + key
	}
}

// dispatchAcrossChain walks an ordered list of upstreams, calling
// dispatchWithKeyRotation on each until one returns success or the
// chain is exhausted. Per-attempt records from every upstream are
// concatenated into the returned dispatchResult.Attempts so the
// audit row (A.6) sees the full forensics — "panshi/key1: 429,
// panshi/key2: 429, claude/default: 200".
//
// Advance rules (DESIGN_WORKSTREAM_A §4):
//   - AttemptOK: return immediately. Final result.
//   - AttemptRateLimited (after key rotation exhausted): advance
//     to next chain step.
//   - AttemptUpstreamError (5xx) / AttemptNetworkError /
//     AttemptTimeout: advance to next chain step.
//   - AttemptClientError (4xx other than 429): return immediately.
//     The request is malformed; trying another upstream won't fix
//     it.
//
// The last attempt in the chain has its FallbackReason left empty
// (no advance happened). Earlier attempts have FallbackReason set
// to "chain_advance" or whatever the per-attempt loop already
// set ("rate_limited_rotate_key", "all_keys_rate_limited").
func dispatchAcrossChain(ctx context.Context, chain []*ResolvedUpstream, body []byte, opts dispatchOpts, log *slog.Logger) dispatchResult {
	if len(chain) == 0 {
		return dispatchResult{Err: errNilUpstream}
	}
	var allAttempts []Attempt
	var lastResult dispatchResult
	for chainIdx, up := range chain {
		stepOpts := opts
		stepOpts.UpstreamName = up.Cfg.Name
		stepResult := dispatchWithKeyRotation(ctx, up, body, stepOpts, log)
		// Stitch this step's attempts onto the running list.
		stepAttempts := stepResult.Attempts
		// Mark the last attempt of this step with the chain
		// advance reason (if we're going to advance).
		if len(stepAttempts) > 0 && chainIdx < len(chain)-1 {
			outcome := stepAttempts[len(stepAttempts)-1].Outcome
			if shouldAdvanceChain(outcome) && stepAttempts[len(stepAttempts)-1].FallbackReason == "" {
				stepAttempts[len(stepAttempts)-1].FallbackReason = "chain_advance:" + string(outcome)
			} else if stepAttempts[len(stepAttempts)-1].FallbackReason != "" {
				// Existing reason ("all_keys_rate_limited") plus
				// chain advance — concatenate so the audit row
				// captures both decisions.
				stepAttempts[len(stepAttempts)-1].FallbackReason += "+chain_advance"
			}
		}
		allAttempts = append(allAttempts, stepAttempts...)
		lastResult = stepResult
		// Decide whether to advance.
		if len(stepAttempts) == 0 {
			// Edge case: dispatchWithKeyRotation returned no
			// attempts (nil-upstream guard). Treat as
			// non-advancing failure.
			break
		}
		finalOutcome := stepAttempts[len(stepAttempts)-1].Outcome
		if !shouldAdvanceChain(finalOutcome) {
			break
		}
	}
	lastResult.Attempts = allAttempts
	return lastResult
}

// shouldAdvanceChain reports whether the chain dispatcher should
// move to the next upstream after observing the given outcome.
func shouldAdvanceChain(o AttemptOutcome) bool {
	switch o {
	case AttemptRateLimited, AttemptUpstreamError, AttemptNetworkError, AttemptTimeout:
		return true
	}
	return false
}
