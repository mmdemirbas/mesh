package gateway

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// GatewayCfg configures a single LLM API gateway instance.
type GatewayCfg struct {
	// Friendly name for this gateway instance.
	Name string `yaml:"name"`
	// Local listening address (e.g., "127.0.0.1:3457").
	Bind string `yaml:"bind"`
	// Upstream endpoint URL. Translation handlers require the API-specific
	// path (/v1/chat/completions or /v1/messages); passthrough handlers use
	// the base URL and preserve the client's request path.
	Upstream string `yaml:"upstream"`
	// API language the upstream server speaks: "anthropic" or "openai".
	UpstreamAPI string `yaml:"upstream_api"`
	// API language this gateway accepts from clients: "anthropic" or "openai".
	ClientAPI string `yaml:"client_api"`
	// Name of the environment variable holding the upstream API key.
	// When unset, the gateway preserves the client's auth headers verbatim
	// (required for OAuth-authenticated clients such as Claude Code).
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// Optional outbound proxy for upstream requests (e.g., "socks5://127.0.0.1:1081").
	Proxy string `yaml:"proxy,omitempty"`
	// Upstream request timeout (e.g., "600s"). Default: "600s".
	Timeout string `yaml:"timeout,omitempty"`
	// Model name remapping: client model name -> upstream model name.
	ModelMap map[string]string `yaml:"model_map,omitempty"`
	// Default max_tokens when the client omits it. Default: 32768.
	// Only applied in translation mode; passthrough does not mutate requests.
	DefaultMaxTokens int `yaml:"default_max_tokens,omitempty"`
	// Optional request/response audit log.
	Log LogCfg `yaml:"log,omitempty"`
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

// Direction returns the derived direction. Callers must Validate first.
func (c *GatewayCfg) Direction() Direction {
	switch {
	case c.ClientAPI == APIAnthropic && c.UpstreamAPI == APIOpenAI:
		return DirA2O
	case c.ClientAPI == APIOpenAI && c.UpstreamAPI == APIAnthropic:
		return DirO2A
	case c.ClientAPI == APIAnthropic && c.UpstreamAPI == APIAnthropic:
		return DirA2A
	default:
		return DirO2O
	}
}

// IsPassthrough reports whether client and upstream APIs match.
func (c *GatewayCfg) IsPassthrough() bool {
	return c.ClientAPI == c.UpstreamAPI
}

// Validate checks that the gateway configuration is well-formed.
func (c *GatewayCfg) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.Bind == "" {
		return fmt.Errorf("bind is required")
	}
	if !isValidAPI(c.ClientAPI) {
		return fmt.Errorf("client_api must be %q or %q, got %q", APIAnthropic, APIOpenAI, c.ClientAPI)
	}
	if !isValidAPI(c.UpstreamAPI) {
		return fmt.Errorf("upstream_api must be %q or %q, got %q", APIAnthropic, APIOpenAI, c.UpstreamAPI)
	}
	if c.Upstream == "" {
		return fmt.Errorf("upstream is required")
	}
	if c.Timeout != "" {
		if _, err := time.ParseDuration(c.Timeout); err != nil {
			return fmt.Errorf("invalid timeout %q: %w", c.Timeout, err)
		}
	}

	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return fmt.Errorf("invalid bind address %q: %w", c.Bind, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("bind address %q must be an explicit loopback IP (127.0.0.1 or ::1)", c.Bind)
	}

	if c.DefaultMaxTokens < 0 {
		return fmt.Errorf("default_max_tokens must be non-negative")
	}
	if err := c.Log.validate(); err != nil {
		return fmt.Errorf("log: %w", err)
	}
	return nil
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
func (c *GatewayCfg) TimeoutDuration() time.Duration {
	if c.Timeout != "" {
		d, _ := time.ParseDuration(c.Timeout)
		return d
	}
	return 600 * time.Second
}

// MaxTokens returns the configured default or 32768.
func (c *GatewayCfg) MaxTokens() int {
	if c.DefaultMaxTokens > 0 {
		return c.DefaultMaxTokens
	}
	return 32768
}

// MapModel applies the model_map to a client-provided model name.
// Returns the mapped name, or the original if no mapping exists.
func (c *GatewayCfg) MapModel(model string) string {
	if mapped, ok := c.ModelMap[model]; ok {
		return mapped
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
