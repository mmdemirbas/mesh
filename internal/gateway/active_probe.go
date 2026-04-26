package gateway

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Workstream A.3 — active health probes.
//
// When active probing is enabled on an upstream, mesh runs one
// goroutine per upstream that fires the configured probe payload at
// the configured interval. The probe outcome feeds the same per-key
// state machinery as a real request (recordPassiveOutcome from
// A.2), so degradation discovered via probe surfaces on the next
// real request as if it had been observed live.
//
// Probes use a randomly-selected key from the pool rather than the
// rotation policy — probe traffic is uniformly distributed across
// keys so the operator can detect "key 3 of 5 is stuck" without
// waiting for the rotation policy to land traffic on it.
//
// See DESIGN_WORKSTREAM_A.local.md §3.3.

// runActiveProbes starts one probe goroutine per upstream that has
// Health.Active.Enabled set. The goroutine runs until ctx.Done() is
// signaled (gateway shutdown). Returns immediately; the goroutines
// own their own lifecycle.
//
// Caller is gateway.Start. Empty router or router with no
// active-probe upstreams is a no-op.
func runActiveProbes(ctx context.Context, router *Router, log *slog.Logger) {
	if router == nil {
		return
	}
	for name, up := range router.upstreams {
		if !up.Cfg.Health.Active.Enabled {
			continue
		}
		if up.Keys == nil || up.Keys.Len() == 0 {
			// No keys to probe with. Active probing on a
			// passthrough upstream is a config error —
			// validation should catch it, but defensive
			// here.
			log.Warn("active health probe skipped: upstream has no keys",
				"upstream", name)
			continue
		}
		go probeLoop(ctx, name, up, log.With("upstream", name))
	}
}

// probeLoop is the per-upstream goroutine. Wakes on the configured
// interval, fires the probe, classifies the outcome, and updates
// the chosen key's state via recordPassiveOutcome.
func probeLoop(ctx context.Context, upstreamName string, up *ResolvedUpstream, log *slog.Logger) {
	interval := activeInterval(up.Cfg.Health.Active)
	timeout := activeTimeout(up.Cfg.Health.Active)
	payload := []byte(up.Cfg.Health.Active.Payload)
	log.Debug("active probe started",
		"interval", interval, "timeout", timeout,
		"keys", up.Keys.Len())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOneProbe(ctx, upstreamName, up, payload, timeout, log)
		}
	}
}

// runOneProbe picks a random key, fires the probe, classifies the
// outcome, and records it. Exposed (lowercase, package-internal)
// for tests so they can drive a single probe synchronously without
// running the full loop. The upstreamName parameter is unused at
// runtime today but retained for future log-correlation use cases
// (e.g., when a single probe surfaces a metric).
func runOneProbe(ctx context.Context, _ string, up *ResolvedUpstream, payload []byte, timeout time.Duration, log *slog.Logger) {
	key := pickProbeKey(up.Keys, time.Now())
	if key == nil {
		// All keys degraded — nothing to probe with. The
		// passive-side state will recover the keys on its own
		// timetable; there's nothing useful to do here.
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	headers := map[string]string{}
	if key.Value != "" {
		// Use the same auth-shape branching as real dispatch so
		// Anthropic probes get x-api-key + anthropic-version, not
		// the (silently 401-rejected) Bearer header. Without this,
		// every Anthropic-upstream probe was misclassified as
		// AttemptClientError and did not contribute to passive
		// health detection.
		applyAuthHeaders(headers, up.Cfg.API, key.Value)
	}
	start := time.Now()
	status, _, err := doUpstreamRequest(probeCtx, up.Client, up.Cfg.Target, payload, headers, log)
	outcome := classifyOutcome(status, err)
	recordPassiveOutcome(key, up.Cfg.Health, outcome, time.Now())
	log.Debug("active probe done",
		"key", key.ID, "status", status,
		"outcome", string(outcome),
		"elapsed", time.Since(start),
		"err", err)
}

// pickProbeKey selects a usable key uniformly at random from the
// pool. Different from the rotation policies — probes deliberately
// don't follow round-robin / sticky rules so probe traffic spreads
// independently of real-request distribution.
func pickProbeKey(pool *KeyPool, now time.Time) *KeyState {
	if pool == nil || pool.Len() == 0 {
		return nil
	}
	usable := make([]*KeyState, 0, pool.Len())
	for _, k := range pool.Keys {
		if k.IsUsable(now) {
			usable = append(usable, k)
		}
	}
	if len(usable) == 0 {
		return nil
	}
	return usable[rand.IntN(len(usable))]
}

// activeInterval returns the effective probe interval, applying the
// default when the field is unset or unparseable.
func activeInterval(c ActiveHealthCfg) time.Duration {
	if c.Interval == "" {
		return defaultActiveProbeInterval
	}
	if d, err := time.ParseDuration(c.Interval); err == nil && d > 0 {
		return d
	}
	return defaultActiveProbeInterval
}

// activeTimeout returns the effective per-probe timeout.
func activeTimeout(c ActiveHealthCfg) time.Duration {
	if c.Timeout == "" {
		return defaultActiveProbeTimeout
	}
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return defaultActiveProbeTimeout
}

