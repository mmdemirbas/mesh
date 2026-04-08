package gateway

import (
	"strings"
	"testing"
)

func TestGatewayCfg_Validate(t *testing.T) {
	valid := GatewayCfg{
		Name:     "test",
		Bind:     "127.0.0.1:3457",
		Mode:     ModeAnthropicToOpenAI,
		Upstream: "https://example.com/v1/chat/completions",
	}

	tests := []struct {
		name    string
		modify  func(*GatewayCfg)
		wantErr string
	}{
		{"valid", func(c *GatewayCfg) {}, ""},
		{"valid_ipv6_loopback", func(c *GatewayCfg) { c.Bind = "[::1]:3457" }, ""},
		{"valid_o2a_mode", func(c *GatewayCfg) { c.Mode = ModeOpenAIToAnthropic }, ""},
		{"empty_name", func(c *GatewayCfg) { c.Name = "" }, "name is required"},
		{"empty_bind", func(c *GatewayCfg) { c.Bind = "" }, "bind is required"},
		{"empty_upstream", func(c *GatewayCfg) { c.Upstream = "" }, "upstream is required"},
		{"invalid_mode", func(c *GatewayCfg) { c.Mode = "bad" }, "mode must be"},
		{"invalid_timeout", func(c *GatewayCfg) { c.Timeout = "not-a-duration" }, "invalid timeout"},
		{"negative_max_tokens", func(c *GatewayCfg) { c.DefaultMaxTokens = -1 }, "default_max_tokens must be non-negative"},
		{"non_loopback_bind", func(c *GatewayCfg) { c.Bind = "0.0.0.0:3457" }, "must be an explicit loopback IP"},
		{"wildcard_bind", func(c *GatewayCfg) { c.Bind = ":3457" }, "must be an explicit loopback IP"},
		{"external_ip_bind", func(c *GatewayCfg) { c.Bind = "192.168.1.1:3457" }, "must be an explicit loopback IP"},
		{"localhost_bind", func(c *GatewayCfg) { c.Bind = "localhost:3457" }, "must be an explicit loopback IP"},
		{"hostname_bind", func(c *GatewayCfg) { c.Bind = "mybox.example.com:3457" }, "must be an explicit loopback IP"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.modify(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestGatewayCfg_MapModel(t *testing.T) {
	cfg := GatewayCfg{ModelMap: map[string]string{"a": "b"}}
	if got := cfg.MapModel("a"); got != "b" {
		t.Errorf("MapModel(a) = %q, want b", got)
	}
	if got := cfg.MapModel("c"); got != "c" {
		t.Errorf("MapModel(c) = %q, want c (passthrough)", got)
	}
}

func TestGatewayCfg_MaxTokens(t *testing.T) {
	cfg := GatewayCfg{}
	if got := cfg.MaxTokens(); got != 32768 {
		t.Errorf("MaxTokens() = %d, want 32768 (default)", got)
	}
	cfg.DefaultMaxTokens = 16384
	if got := cfg.MaxTokens(); got != 16384 {
		t.Errorf("MaxTokens() = %d, want 16384", got)
	}
}

func TestGatewayCfg_TimeoutDuration(t *testing.T) {
	cfg := GatewayCfg{}
	if got := cfg.TimeoutDuration().Seconds(); got != 600 {
		t.Errorf("TimeoutDuration() = %f, want 600", got)
	}
	cfg.Timeout = "30s"
	if got := cfg.TimeoutDuration().Seconds(); got != 30 {
		t.Errorf("TimeoutDuration() = %f, want 30", got)
	}
}

