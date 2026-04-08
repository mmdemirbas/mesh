package gateway

import (
	"fmt"
	"net"
	"time"
)

// GatewayCfg configures a single LLM API gateway instance.
type GatewayCfg struct {
	// Friendly name for this gateway instance.
	Name string `yaml:"name"`
	// Local listening address (e.g., "127.0.0.1:3457").
	Bind string `yaml:"bind"`
	// Translation direction: "anthropic-to-openai" or "openai-to-anthropic".
	Mode string `yaml:"mode"`
	// Upstream endpoint URL (e.g., "https://oneapi.rnd.huawei.com/v1/chat/completions").
	Upstream string `yaml:"upstream"`
	// Name of the environment variable holding the upstream API key.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// Optional outbound proxy for upstream requests (e.g., "socks5://127.0.0.1:1081").
	Proxy string `yaml:"proxy,omitempty"`
	// Upstream request timeout (e.g., "600s"). Default: "600s".
	Timeout string `yaml:"timeout,omitempty"`
	// Model name remapping: client model name -> upstream model name.
	ModelMap map[string]string `yaml:"model_map,omitempty"`
	// Default max_tokens when the client omits it. Default: 32768.
	DefaultMaxTokens int `yaml:"default_max_tokens,omitempty"`
}

const (
	ModeAnthropicToOpenAI = "anthropic-to-openai"
	ModeOpenAIToAnthropic = "openai-to-anthropic"
)

// Validate checks that the gateway configuration is well-formed.
func (c *GatewayCfg) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.Bind == "" {
		return fmt.Errorf("bind is required")
	}
	if c.Mode != ModeAnthropicToOpenAI && c.Mode != ModeOpenAIToAnthropic {
		return fmt.Errorf("mode must be %q or %q, got %q", ModeAnthropicToOpenAI, ModeOpenAIToAnthropic, c.Mode)
	}
	if c.Upstream == "" {
		return fmt.Errorf("upstream is required")
	}
	if c.Timeout != "" {
		if _, err := time.ParseDuration(c.Timeout); err != nil {
			return fmt.Errorf("invalid timeout %q: %w", c.Timeout, err)
		}
	}

	// Refuse non-loopback bind addresses.
	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return fmt.Errorf("invalid bind address %q: %w", c.Bind, err)
	}
	if host != "" && host != "localhost" {
		ip := net.ParseIP(host)
		if ip != nil && !ip.IsLoopback() {
			return fmt.Errorf("bind address %q is not loopback; gateway must bind to 127.0.0.1 or ::1", c.Bind)
		}
	}

	if c.DefaultMaxTokens < 0 {
		return fmt.Errorf("default_max_tokens must be non-negative")
	}
	return nil
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
