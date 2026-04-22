package gateway

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// Router resolves a client model name to the appropriate upstream.
type Router struct {
	rules     []RoutingRule
	upstreams map[string]*ResolvedUpstream
}

// ResolvedUpstream is a pre-resolved upstream with its HTTP client, API key, etc.
type ResolvedUpstream struct {
	Cfg    UpstreamCfg
	Client *http.Client
	APIKey string
}

// NewRouter builds a Router from the gateway configuration. It creates
// an HTTP client (with optional proxy) and resolves the API key for each
// upstream.
func NewRouter(cfg *GatewayCfg) (*Router, error) {
	upstreams := make(map[string]*ResolvedUpstream, len(cfg.Upstream))

	for _, u := range cfg.Upstream {
		transport := &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}

		if u.Proxy != "" {
			proxyURL, err := url.Parse(u.Proxy)
			if err != nil {
				return nil, fmt.Errorf("upstream %q: invalid proxy URL %q: %w", u.Name, u.Proxy, err)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
		}

		apiKey := ""
		if u.APIKeyEnv != "" {
			apiKey = os.Getenv(u.APIKeyEnv)
		}

		upstreams[u.Name] = &ResolvedUpstream{
			Cfg: u,
			Client: &http.Client{
				Transport: transport,
				Timeout:   u.TimeoutDuration(),
			},
			APIKey: apiKey,
		}
	}

	return &Router{
		rules:     cfg.Routing,
		upstreams: upstreams,
	}, nil
}

// Route returns the resolved upstream for the given model name.
// Rules are evaluated in order; for each rule, patterns are checked using
// the same glob matching as MapModel: exact → glob (longest first) → "*" catch-all.
// Returns nil if no routing rule matches.
func (r *Router) Route(model string) *ResolvedUpstream {
	for _, rule := range r.rules {
		if matchesAnyPattern(model, rule.ClientModel) {
			return r.upstreams[rule.UpstreamName]
		}
	}
	return nil
}

// Upstream returns a named upstream. Used by passthrough mode where
// routing is not model-based but the upstream is pre-selected.
func (r *Router) Upstream(name string) *ResolvedUpstream {
	return r.upstreams[name]
}

// DefaultUpstream returns the first upstream that would match a wildcard
// pattern. Used as a fallback when the request body cannot be parsed.
func (r *Router) DefaultUpstream() *ResolvedUpstream {
	for _, rule := range r.rules {
		for _, pattern := range rule.ClientModel {
			if pattern == "*" {
				return r.upstreams[rule.UpstreamName]
			}
		}
	}
	// No wildcard rule; return the first upstream as ultimate fallback.
	for _, u := range r.upstreams {
		return u
	}
	return nil
}

// matchesAnyPattern checks if model matches any of the given patterns.
// Matching order: exact → glob (longest first) → "*" catch-all.
func matchesAnyPattern(model string, patterns []string) bool {
	// Phase 1: exact match
	for _, p := range patterns {
		if !strings.ContainsAny(p, "*?[") && p == model {
			return true
		}
	}

	// Phase 2: glob patterns (longest first)
	type entry struct {
		pattern string
	}
	var globs []entry
	var hasCatchAll bool
	for _, p := range patterns {
		if p == "*" {
			hasCatchAll = true
		} else if strings.ContainsAny(p, "*?[") {
			globs = append(globs, entry{p})
		}
	}

	// Sort by pattern length descending (longer = more specific)
	for i := 0; i < len(globs); i++ {
		for j := i + 1; j < len(globs); j++ {
			if len(globs[j].pattern) > len(globs[i].pattern) {
				globs[i], globs[j] = globs[j], globs[i]
			}
		}
	}

	for _, g := range globs {
		if matched, _ := path.Match(g.pattern, model); matched {
			return true
		}
	}

	if hasCatchAll {
		return true
	}

	return false
}
