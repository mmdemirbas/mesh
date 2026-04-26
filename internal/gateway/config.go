package gateway

import (
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GatewayCfg configures a compound LLM API gateway with multiple clients,
// routing rules, and upstream targets.
type GatewayCfg struct {
	// Friendly name for this gateway instance.
	Name string `yaml:"name"`
	// Optional request/response audit log.
	Log LogCfg `yaml:"log,omitempty"`
	// Local listeners for incoming requests.
	Client []ClientCfg `yaml:"client"`
	// Model-based routing rules. Evaluated in order; first match wins.
	Routing []RoutingRule `yaml:"routing"`
	// Upstream targets.
	Upstream []UpstreamCfg `yaml:"upstream"`
}

// ClientCfg defines a local listener for incoming requests.
type ClientCfg struct {
	// Local listening address (e.g., "127.0.0.1:3457").
	Bind string `yaml:"bind"`
	// API language this listener accepts: "anthropic" or "openai".
	API string `yaml:"api"`
}

// RoutingRule maps client model patterns to an upstream name (or
// an ordered chain of upstream names for fallback). Rules are
// evaluated in order; first match wins.
type RoutingRule struct {
	// Name of the upstream to route matching requests to. Use this
	// for the common single-upstream case. Mutually exclusive with
	// UpstreamChain.
	UpstreamName string `yaml:"upstream_name,omitempty"`
	// UpstreamChain is the multi-upstream form: an ordered list of
	// upstream names. Mesh tries each in order, advancing on
	// per-upstream "all keys exhausted" / 5xx / network failure.
	// Single-element chains are equivalent to UpstreamName.
	// Mutually exclusive with UpstreamName.
	//
	// v1 constraint: all chain elements must share the same `api`
	// value (mesh translates the request once per rule, not per
	// chain step). Cross-API chains are out of scope until a
	// future workstream adds per-step translation.
	UpstreamChain []string `yaml:"upstream_chain,omitempty"`
	// Glob patterns for client model names. "*" matches all models.
	// Uses path.Match semantics (* matches any sequence, ? matches one char, [...] brackets).
	ClientModel []string `yaml:"client_model"`
}

// resolvedUpstreamChain returns the ordered list of upstream names
// for this rule, treating UpstreamName as a one-element chain.
// Caller owns the returned slice (a defensive copy).
func (r RoutingRule) resolvedUpstreamChain() []string {
	if len(r.UpstreamChain) > 0 {
		out := make([]string, len(r.UpstreamChain))
		copy(out, r.UpstreamChain)
		return out
	}
	if r.UpstreamName != "" {
		return []string{r.UpstreamName}
	}
	return nil
}

// UpstreamCfg defines a single upstream target.
type UpstreamCfg struct {
	// Unique name for this upstream.
	Name string `yaml:"name"`
	// Upstream endpoint URL. Translation handlers require the API-specific
	// path (/v1/chat/completions or /v1/messages); passthrough handlers use
	// the base URL and preserve the client's request path.
	Target string `yaml:"target"`
	// API language the upstream server speaks: "anthropic" or "openai".
	API string `yaml:"api"`
	// Name of the environment variable holding the upstream API key.
	// When unset, the gateway preserves the client's auth headers verbatim
	// (required for OAuth-authenticated clients such as Claude Code).
	// Mutually exclusive with APIKeyEnvs; use one form or the other.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// APIKeyEnvs is the multi-key form of APIKeyEnv. Each entry names
	// an environment variable; at startup mesh resolves all of them
	// into a key pool. Per-request key selection is governed by
	// RotationPolicy. Mutually exclusive with APIKeyEnv.
	//
	// Single-element lists are equivalent to APIKeyEnv with the same
	// value (the rotation policy short-circuits to "single" for any
	// pool of size ≤ 1).
	APIKeyEnvs []string `yaml:"api_key_envs,omitempty"`
	// RotationPolicy picks the key for each request out of the
	// multi-key pool. Valid: "round_robin" (default for multi-key
	// configs), "lru", "sticky_session". Ignored for single-key
	// pools. See DESIGN_WORKSTREAM_A.local.md §2.
	RotationPolicy string `yaml:"rotation_policy,omitempty"`
	// Health controls passive (always-on by default) and active
	// (opt-in, A.3) health checks. See DESIGN_WORKSTREAM_A.local.md
	// §1.2 + §3. Zero value = passive enabled with default
	// thresholds; active disabled.
	Health HealthCfg `yaml:"health,omitempty"`
	// Optional outbound proxy for upstream requests (e.g., "socks5://127.0.0.1:1081").
	Proxy string `yaml:"proxy,omitempty"`
	// Upstream request timeout (e.g., "600s"). Default: "600s".
	Timeout string `yaml:"timeout,omitempty"`
	// Default max_tokens when the client omits it. Default: 32768.
	// Only applied in translation mode; passthrough does not mutate requests.
	DefaultMaxTokens int `yaml:"default_max_tokens,omitempty"`
	// Maximum input context window in tokens. When set, the gateway estimates
	// incoming request token count and rejects or summarizes requests that
	// would exceed this limit. Zero means unlimited (default).
	ContextWindow int `yaml:"context_window,omitempty"`
	// Name of another upstream to use for summarization when a request
	// exceeds ContextWindow. Must reference an upstream defined in the same
	// gateway config. When empty and ContextWindow is exceeded, the request
	// is rejected with a clear error message.
	Summarizer string `yaml:"summarizer,omitempty"`
	// Model name remapping: client model name -> upstream model name.
	ModelMap map[string]string `yaml:"model_map,omitempty"`
}

// LogCfg configures per-gateway audit logging. The zero value disables logging
// at runtime: Level defaults to "metadata" when any other Log field is set,
// otherwise the recorder is a no-op.
type LogCfg struct {
	// Verbosity: "off", "metadata", or "full". Default: "metadata".
	// "metadata" records request/response shape (model, tokens, latency,
	// outcome) without bodies. "full" additionally records request and
	// response bodies (reassembled for streamed responses).
	Level string `yaml:"level,omitempty"`
	// Directory for JSONL audit files. Default: "~/.mesh/gateway".
	// Each gateway writes to <dir>/<gateway-name>/YYYY-MM-DD.jsonl.
	Dir string `yaml:"dir,omitempty"`
	// Rollover threshold for a single audit file, e.g., "100MB". Default: "100MB".
	MaxFileSize string `yaml:"max_file_size,omitempty"`
	// Age at which old audit files are deleted, e.g., "720h" (30 days).
	// Default: "720h". Accepts any duration parseable by time.ParseDuration.
	MaxAge string `yaml:"max_age,omitempty"`
}

const (
	APIAnthropic = "anthropic"
	APIOpenAI    = "openai"

	LogLevelOff      = "off"
	LogLevelMetadata = "metadata"
	LogLevelFull     = "full"

	defaultLogDir         = "~/.mesh/gateway"
	defaultLogMaxFileSize = "100MB"
	defaultLogMaxAge      = "30d"
)

// Direction is the derived (ClientAPI, UpstreamAPI) pair.
type Direction int

const (
	// DirA2O: client speaks Anthropic, upstream speaks OpenAI (translate).
	DirA2O Direction = iota
	// DirO2A: client speaks OpenAI, upstream speaks Anthropic (translate).
	DirO2A
	// DirA2A: both sides Anthropic (transparent passthrough).
	DirA2A
	// DirO2O: both sides OpenAI (transparent passthrough).
	DirO2O
)

// String returns a short tag used in logs and audit records.
func (d Direction) String() string {
	switch d {
	case DirA2O:
		return "a2o"
	case DirO2A:
		return "o2a"
	case DirA2A:
		return "a2a"
	case DirO2O:
		return "o2o"
	}
	return "unknown"
}

// ResolveDirection returns the direction for a given (clientAPI, upstreamAPI) pair.
func ResolveDirection(clientAPI, upstreamAPI string) Direction {
	switch {
	case clientAPI == APIAnthropic && upstreamAPI == APIOpenAI:
		return DirA2O
	case clientAPI == APIOpenAI && upstreamAPI == APIAnthropic:
		return DirO2A
	case clientAPI == APIAnthropic && upstreamAPI == APIAnthropic:
		return DirA2A
	default:
		return DirO2O
	}
}

// IsPassthrough reports whether the direction is same-API passthrough.
func (d Direction) IsPassthrough() bool {
	return d == DirA2A || d == DirO2O
}

// Validate checks that the gateway configuration is well-formed.
func (c *GatewayCfg) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}

	// --- Clients ---
	if len(c.Client) == 0 {
		return fmt.Errorf("at least one client is required")
	}
	for i, cl := range c.Client {
		if cl.Bind == "" {
			return fmt.Errorf("client[%d]: bind is required", i)
		}
		if !isValidAPI(cl.API) {
			return fmt.Errorf("client[%d]: api must be %q or %q, got %q", i, APIAnthropic, APIOpenAI, cl.API)
		}
		host, _, err := net.SplitHostPort(cl.Bind)
		if err != nil {
			return fmt.Errorf("client[%d]: invalid bind address %q: %w", i, cl.Bind, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("client[%d]: bind address %q must be an explicit loopback IP (127.0.0.1 or ::1)", i, cl.Bind)
		}
	}

	// --- Upstreams ---
	if len(c.Upstream) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	upstreamNames := make(map[string]int, len(c.Upstream))
	for i, u := range c.Upstream {
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if prev, ok := upstreamNames[u.Name]; ok {
			return fmt.Errorf("duplicate upstream name %q: upstream[%d] and upstream[%d]", u.Name, prev, i)
		}
		upstreamNames[u.Name] = i

		if u.Target == "" {
			return fmt.Errorf("upstream[%d] %q: target is required", i, u.Name)
		}
		if !isValidAPI(u.API) {
			return fmt.Errorf("upstream[%d] %q: api must be %q or %q, got %q", i, u.Name, APIAnthropic, APIOpenAI, u.API)
		}
		if u.Timeout != "" {
			if _, err := time.ParseDuration(u.Timeout); err != nil {
				return fmt.Errorf("upstream[%d] %q: invalid timeout %q: %w", i, u.Name, u.Timeout, err)
			}
		}
		if u.DefaultMaxTokens < 0 {
			return fmt.Errorf("upstream[%d] %q: default_max_tokens must be non-negative", i, u.Name)
		}
		if u.ContextWindow < 0 {
			return fmt.Errorf("upstream[%d] %q: context_window must be non-negative", i, u.Name)
		}
		if u.Summarizer != "" && u.Summarizer == u.Name {
			return fmt.Errorf("upstream[%d] %q: summarizer must not reference itself", i, u.Name)
		}
		// Workstream A.1: multi-key + rotation policy.
		if u.APIKeyEnv != "" && len(u.APIKeyEnvs) > 0 {
			return fmt.Errorf("upstream[%d] %q: api_key_env and api_key_envs are mutually exclusive; use one form", i, u.Name)
		}
		for j, ev := range u.APIKeyEnvs {
			if ev == "" {
				return fmt.Errorf("upstream[%d] %q: api_key_envs[%d] is empty", i, u.Name, j)
			}
		}
		// Detect duplicate env var names within the same pool —
		// they would be indistinguishable at runtime and indicate
		// a config typo.
		if len(u.APIKeyEnvs) > 1 {
			seen := make(map[string]int, len(u.APIKeyEnvs))
			for j, ev := range u.APIKeyEnvs {
				if prev, ok := seen[ev]; ok {
					return fmt.Errorf("upstream[%d] %q: api_key_envs[%d] duplicates api_key_envs[%d] %q", i, u.Name, j, prev, ev)
				}
				seen[ev] = j
			}
		}
		if !IsValidRotationPolicy(u.RotationPolicy) {
			return fmt.Errorf("upstream[%d] %q: invalid rotation_policy %q (valid: round_robin, lru, sticky_session)", i, u.Name, u.RotationPolicy)
		}
		if u.RotationPolicy != "" && len(u.APIKeyEnvs) == 0 && u.APIKeyEnv != "" {
			// Configured policy on a single-key pool is silently
			// ignored at runtime (single-key pools always use
			// "single"). Reject at config-time so the operator
			// notices the unused field.
			return fmt.Errorf("upstream[%d] %q: rotation_policy is set but api_key_envs is not (rotation only applies to multi-key pools; remove the policy or add a second key)", i, u.Name)
		}
		// Workstream A.2: health block validation. The block's own
		// validate() handles the per-field shape; this returns the
		// upstream-context error.
		if err := u.Health.validate(fmt.Sprintf("upstream[%d] %q", i, u.Name)); err != nil {
			return err
		}
		for pattern := range u.ModelMap {
			if strings.ContainsAny(pattern, "*?[") && pattern != "*" {
				if _, err := path.Match(pattern, ""); err != nil {
					return fmt.Errorf("upstream[%d] %q: model_map: invalid glob pattern %q: %w", i, u.Name, pattern, err)
				}
			}
		}
	}

	// Cross-reference: summarizer must point to an existing upstream.
	for i, u := range c.Upstream {
		if u.Summarizer != "" {
			idx, ok := upstreamNames[u.Summarizer]
			if !ok {
				return fmt.Errorf("upstream[%d] %q: summarizer %q does not match any upstream", i, u.Name, u.Summarizer)
			}
			if c.Upstream[idx].API != APIOpenAI {
				return fmt.Errorf("upstream[%d] %q: summarizer %q must use api %q", i, u.Name, u.Summarizer, APIOpenAI)
			}
		}
	}

	// --- Routing ---
	if len(c.Routing) == 0 {
		return fmt.Errorf("at least one routing rule is required")
	}
	for i, r := range c.Routing {
		// Workstream A.5: upstream_name and upstream_chain are
		// mutually exclusive forms of the same field. Either one
		// must be set.
		hasName := r.UpstreamName != ""
		hasChain := len(r.UpstreamChain) > 0
		if hasName && hasChain {
			return fmt.Errorf("routing[%d]: upstream_name and upstream_chain are mutually exclusive; use one form", i)
		}
		if !hasName && !hasChain {
			return fmt.Errorf("routing[%d]: one of upstream_name or upstream_chain is required", i)
		}
		// Resolve the chain (single-name or list).
		chain := r.resolvedUpstreamChain()
		seenInChain := make(map[string]int, len(chain))
		var chainAPI string
		for j, name := range chain {
			if name == "" {
				return fmt.Errorf("routing[%d]: upstream_chain[%d] is empty", i, j)
			}
			idx, ok := upstreamNames[name]
			if !ok {
				if hasChain {
					return fmt.Errorf("routing[%d]: upstream_chain[%d] %q does not match any upstream", i, j, name)
				}
				return fmt.Errorf("routing[%d]: upstream_name %q does not match any upstream", i, name)
			}
			if prev, dup := seenInChain[name]; dup {
				return fmt.Errorf("routing[%d]: upstream_chain[%d] duplicates upstream_chain[%d] %q", i, j, prev, name)
			}
			seenInChain[name] = j
			// All chain elements must share the same api shape
			// (v1 limitation; cross-API chains require per-step
			// translation which is a future workstream).
			api := c.Upstream[idx].API
			if chainAPI == "" {
				chainAPI = api
			} else if api != chainAPI {
				return fmt.Errorf("routing[%d]: upstream_chain mixes APIs (%q has api=%q but earlier elements have api=%q); v1 chains require homogeneous APIs", i, name, api, chainAPI)
			}
		}
		if len(r.ClientModel) == 0 {
			return fmt.Errorf("routing[%d]: at least one client_model pattern is required", i)
		}
		for _, pattern := range r.ClientModel {
			if strings.ContainsAny(pattern, "*?[") && pattern != "*" {
				if _, err := path.Match(pattern, ""); err != nil {
					return fmt.Errorf("routing[%d]: invalid glob pattern %q: %w", i, pattern, err)
				}
			}
		}
	}

	// --- Log ---
	if err := c.Log.validate(); err != nil {
		return fmt.Errorf("log: %w", err)
	}
	return nil
}

// ClientBinds returns all bind addresses from the client list.
func (c *GatewayCfg) ClientBinds() []string {
	binds := make([]string, len(c.Client))
	for i, cl := range c.Client {
		binds[i] = cl.Bind
	}
	return binds
}

func isValidAPI(v string) bool {
	return v == APIAnthropic || v == APIOpenAI
}

func (l *LogCfg) validate() error {
	if l.Level != "" && l.Level != LogLevelOff && l.Level != LogLevelMetadata && l.Level != LogLevelFull {
		return fmt.Errorf("level must be %q, %q, or %q, got %q", LogLevelOff, LogLevelMetadata, LogLevelFull, l.Level)
	}
	if l.MaxFileSize != "" {
		if _, err := ParseSize(l.MaxFileSize); err != nil {
			return fmt.Errorf("invalid max_file_size %q: %w", l.MaxFileSize, err)
		}
	}
	if l.MaxAge != "" {
		if _, err := parseExtendedDuration(l.MaxAge); err != nil {
			return fmt.Errorf("invalid max_age %q: %w", l.MaxAge, err)
		}
	}
	return nil
}

// parseExtendedDuration accepts any stdlib time.ParseDuration input plus
// "Nd" (days) and "Nw" (weeks). Mixed units ("1w2d") are not supported.
func parseExtendedDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	switch last := s[len(s)-1]; last {
	case 'd', 'w':
		num, err := strconv.ParseInt(strings.TrimSpace(s[:len(s)-1]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("not a duration: %q", s)
		}
		if num < 0 {
			return 0, fmt.Errorf("negative duration: %q", s)
		}
		unit := 24 * time.Hour
		if last == 'w' {
			unit = 7 * 24 * time.Hour
		}
		return time.Duration(num) * unit, nil
	}
	return time.ParseDuration(s)
}

// ResolvedLevel returns the effective logging level. A fully zero LogCfg
// (no log: block in YAML) is silent — Level defaults to "metadata" only when
// the user explicitly configured some other log field. This keeps gateways
// with no log configuration from writing audit files.
func (l *LogCfg) ResolvedLevel() string {
	if l.Level != "" {
		return l.Level
	}
	if l.Dir != "" || l.MaxFileSize != "" || l.MaxAge != "" {
		return LogLevelMetadata
	}
	return LogLevelOff
}

// ResolvedDir returns the effective audit directory, applying the default.
func (l *LogCfg) ResolvedDir() string {
	if l.Dir == "" {
		return defaultLogDir
	}
	return l.Dir
}

// ResolvedMaxFileSize returns the effective rollover threshold in bytes.
func (l *LogCfg) ResolvedMaxFileSize() int64 {
	v := l.MaxFileSize
	if v == "" {
		v = defaultLogMaxFileSize
	}
	n, _ := ParseSize(v)
	return n
}

// ResolvedMaxAge returns the effective retention window.
func (l *LogCfg) ResolvedMaxAge() time.Duration {
	v := l.MaxAge
	if v == "" {
		v = defaultLogMaxAge
	}
	d, _ := parseExtendedDuration(v)
	return d
}

// TimeoutDuration returns the parsed timeout or the default (600s).
func (u *UpstreamCfg) TimeoutDuration() time.Duration {
	if u.Timeout != "" {
		d, _ := time.ParseDuration(u.Timeout)
		return d
	}
	return 600 * time.Second
}

// MaxTokens returns the configured default or 32768.
func (u *UpstreamCfg) MaxTokens() int {
	if u.DefaultMaxTokens > 0 {
		return u.DefaultMaxTokens
	}
	return 32768
}

// HasContextLimit reports whether this upstream has a context window constraint.
func (u *UpstreamCfg) HasContextLimit() bool {
	return u.ContextWindow > 0
}

// MapModel applies the model_map to a client-provided model name.
// Matching order: exact literal → glob patterns (longest first) → "*" catch-all → passthrough.
// Glob patterns use path.Match semantics (* matches any sequence, ? matches one char, [...] brackets).
func (u *UpstreamCfg) MapModel(model string) string {
	// Exact literal match (O(1)).
	if mapped, ok := u.ModelMap[model]; ok {
		if !strings.ContainsAny(model, "*?[") {
			return mapped
		}
	}

	// Collect glob patterns, longest first (more specific wins).
	type entry struct {
		pattern, target string
	}
	var globs []entry
	var catchAll string
	var hasCatchAll bool
	for k, v := range u.ModelMap {
		if k == "*" {
			catchAll, hasCatchAll = v, true
		} else if strings.ContainsAny(k, "*?[") {
			globs = append(globs, entry{k, v})
		}
	}
	sort.Slice(globs, func(i, j int) bool {
		return len(globs[i].pattern) > len(globs[j].pattern)
	})
	for _, g := range globs {
		if matched, _ := path.Match(g.pattern, model); matched {
			return g.target
		}
	}
	if hasCatchAll {
		return catchAll
	}
	return model
}

// ParseSize parses a size string like "100MB", "1GB", "512K", or a raw byte
// count. Recognized suffixes: B, K/KB, M/MB, G/GB (powers of 1024).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(upper, "GB"):
		mult = 1 << 30
		upper = strings.TrimSuffix(upper, "GB")
	case strings.HasSuffix(upper, "MB"):
		mult = 1 << 20
		upper = strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "KB"):
		mult = 1 << 10
		upper = strings.TrimSuffix(upper, "KB")
	case strings.HasSuffix(upper, "G"):
		mult = 1 << 30
		upper = strings.TrimSuffix(upper, "G")
	case strings.HasSuffix(upper, "M"):
		mult = 1 << 20
		upper = strings.TrimSuffix(upper, "M")
	case strings.HasSuffix(upper, "K"):
		mult = 1 << 10
		upper = strings.TrimSuffix(upper, "K")
	case strings.HasSuffix(upper, "B"):
		upper = strings.TrimSuffix(upper, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size: %q", s)
	}
	return n * mult, nil
}
