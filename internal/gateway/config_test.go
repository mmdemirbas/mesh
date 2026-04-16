package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestGatewayCfg_Validate(t *testing.T) {
	t.Parallel()
	valid := GatewayCfg{
		Name:        "test",
		Bind:        "127.0.0.1:3457",
		ClientAPI:   APIAnthropic,
		UpstreamAPI: APIOpenAI,
		Upstream:    "https://example.com/v1/chat/completions",
	}

	tests := []struct {
		name    string
		modify  func(*GatewayCfg)
		wantErr string
	}{
		{"valid_a2o", func(c *GatewayCfg) {}, ""},
		{"valid_o2a", func(c *GatewayCfg) { c.ClientAPI = APIOpenAI; c.UpstreamAPI = APIAnthropic }, ""},
		{"valid_a2a_passthrough", func(c *GatewayCfg) { c.ClientAPI = APIAnthropic; c.UpstreamAPI = APIAnthropic }, ""},
		{"valid_o2o_passthrough", func(c *GatewayCfg) { c.ClientAPI = APIOpenAI; c.UpstreamAPI = APIOpenAI }, ""},
		{"valid_ipv6_loopback", func(c *GatewayCfg) { c.Bind = "[::1]:3457" }, ""},
		{"empty_name", func(c *GatewayCfg) { c.Name = "" }, "name is required"},
		{"empty_bind", func(c *GatewayCfg) { c.Bind = "" }, "bind is required"},
		{"empty_upstream", func(c *GatewayCfg) { c.Upstream = "" }, "upstream is required"},
		{"empty_client_api", func(c *GatewayCfg) { c.ClientAPI = "" }, "client_api must be"},
		{"empty_upstream_api", func(c *GatewayCfg) { c.UpstreamAPI = "" }, "upstream_api must be"},
		{"invalid_client_api", func(c *GatewayCfg) { c.ClientAPI = "bad" }, "client_api must be"},
		{"invalid_upstream_api", func(c *GatewayCfg) { c.UpstreamAPI = "bad" }, "upstream_api must be"},
		{"invalid_timeout", func(c *GatewayCfg) { c.Timeout = "not-a-duration" }, "invalid timeout"},
		{"negative_max_tokens", func(c *GatewayCfg) { c.DefaultMaxTokens = -1 }, "default_max_tokens must be non-negative"},
		{"non_loopback_bind", func(c *GatewayCfg) { c.Bind = "0.0.0.0:3457" }, "must be an explicit loopback IP"},
		{"wildcard_bind", func(c *GatewayCfg) { c.Bind = ":3457" }, "must be an explicit loopback IP"},
		{"external_ip_bind", func(c *GatewayCfg) { c.Bind = "192.168.1.1:3457" }, "must be an explicit loopback IP"},
		{"localhost_bind", func(c *GatewayCfg) { c.Bind = "localhost:3457" }, "must be an explicit loopback IP"},
		{"hostname_bind", func(c *GatewayCfg) { c.Bind = "mybox.example.com:3457" }, "must be an explicit loopback IP"},
		{"invalid_log_level", func(c *GatewayCfg) { c.Log.Level = "chatty" }, "level must be"},
		{"invalid_log_max_file_size", func(c *GatewayCfg) { c.Log.MaxFileSize = "huge" }, "invalid max_file_size"},
		{"invalid_log_max_age", func(c *GatewayCfg) { c.Log.MaxAge = "forever" }, "invalid max_age"},
		{"valid_log_full", func(c *GatewayCfg) { c.Log.Level = LogLevelFull }, ""},
		{"valid_log_off", func(c *GatewayCfg) { c.Log.Level = LogLevelOff }, ""},
		{"valid_log_sizes", func(c *GatewayCfg) {
			c.Log.Level = LogLevelFull
			c.Log.MaxFileSize = "50MB"
			c.Log.MaxAge = "168h"
		}, ""},
		{"valid_model_map_glob", func(c *GatewayCfg) {
			c.ModelMap = map[string]string{"claude-*": "gpt-4o", "*": "default"}
		}, ""},
		{"invalid_model_map_glob", func(c *GatewayCfg) {
			c.ModelMap = map[string]string{"claude-[": "bad"}
		}, "invalid glob pattern"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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

func TestGatewayCfg_Direction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		client, upstream string
		want             Direction
		wantStr          string
		wantPassthrough  bool
	}{
		{APIAnthropic, APIOpenAI, DirA2O, "a2o", false},
		{APIOpenAI, APIAnthropic, DirO2A, "o2a", false},
		{APIAnthropic, APIAnthropic, DirA2A, "a2a", true},
		{APIOpenAI, APIOpenAI, DirO2O, "o2o", true},
	}
	for _, tt := range tests {
		t.Run(tt.wantStr, func(t *testing.T) {
			t.Parallel()
			cfg := GatewayCfg{ClientAPI: tt.client, UpstreamAPI: tt.upstream}
			if got := cfg.Direction(); got != tt.want {
				t.Errorf("Direction() = %v, want %v", got, tt.want)
			}
			if got := cfg.Direction().String(); got != tt.wantStr {
				t.Errorf("Direction().String() = %q, want %q", got, tt.wantStr)
			}
			if got := cfg.IsPassthrough(); got != tt.wantPassthrough {
				t.Errorf("IsPassthrough() = %v, want %v", got, tt.wantPassthrough)
			}
		})
	}
}

func TestLogCfg_Resolved(t *testing.T) {
	t.Parallel()
	t.Run("empty_cfg_is_silent", func(t *testing.T) {
		t.Parallel()
		var l LogCfg
		if got := l.ResolvedLevel(); got != LogLevelOff {
			t.Errorf("ResolvedLevel on empty LogCfg = %q, want %q (no log: block should be silent)", got, LogLevelOff)
		}
	})
	t.Run("defaults_when_any_field_set", func(t *testing.T) {
		t.Parallel()
		l := LogCfg{Dir: "/tmp/gw"}
		if got := l.ResolvedLevel(); got != LogLevelMetadata {
			t.Errorf("ResolvedLevel with dir set = %q, want %q (partial log block defaults to metadata)", got, LogLevelMetadata)
		}
	})
	t.Run("resolved_helpers", func(t *testing.T) {
		t.Parallel()
		var l LogCfg
		if got := l.ResolvedDir(); got != defaultLogDir {
			t.Errorf("ResolvedDir default = %q, want %q", got, defaultLogDir)
		}
		if got := l.ResolvedMaxFileSize(); got != 100*1024*1024 {
			t.Errorf("ResolvedMaxFileSize default = %d, want %d", got, 100*1024*1024)
		}
		if got := l.ResolvedMaxAge().Hours(); got != 720 {
			t.Errorf("ResolvedMaxAge default = %v, want 720h", got)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		t.Parallel()
		l := LogCfg{Level: LogLevelFull, Dir: "/tmp/audit", MaxFileSize: "1GB", MaxAge: "24h"}
		if got := l.ResolvedLevel(); got != LogLevelFull {
			t.Errorf("ResolvedLevel = %q", got)
		}
		if got := l.ResolvedDir(); got != "/tmp/audit" {
			t.Errorf("ResolvedDir = %q", got)
		}
		if got := l.ResolvedMaxFileSize(); got != 1<<30 {
			t.Errorf("ResolvedMaxFileSize = %d, want %d", got, 1<<30)
		}
		if got := l.ResolvedMaxAge().Hours(); got != 24 {
			t.Errorf("ResolvedMaxAge = %v, want 24h", got)
		}
	})
}

func TestParseExtendedDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"15m", 15 * time.Minute, false},
		{"0d", 0, false},
		{" 7d ", 7 * 24 * time.Hour, false},
		{"", 0, true},
		{"-3d", 0, true},
		{"1w2d", 0, true}, // mixed units not supported
		{"forever", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseExtendedDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseExtendedDuration(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseExtendedDuration(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseExtendedDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLogCfg_MaxAgeAcceptsDaysAndWeeks(t *testing.T) {
	t.Parallel()
	cfg := GatewayCfg{
		Name:        "gw",
		Bind:        "127.0.0.1:0",
		ClientAPI:   APIAnthropic,
		UpstreamAPI: APIAnthropic,
		Upstream:    "https://api.anthropic.com",
		Log:         LogCfg{Level: LogLevelFull, MaxAge: "30d"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("30d should validate: %v", err)
	}
	if got := cfg.Log.ResolvedMaxAge(); got != 30*24*time.Hour {
		t.Errorf("ResolvedMaxAge(30d) = %v, want 720h", got)
	}
	cfg.Log.MaxAge = "2w"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("2w should validate: %v", err)
	}
	if got := cfg.Log.ResolvedMaxAge(); got != 14*24*time.Hour {
		t.Errorf("ResolvedMaxAge(2w) = %v, want 336h", got)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"100", 100, false},
		{"512B", 512, false},
		{"4K", 4 << 10, false},
		{"4KB", 4 << 10, false},
		{"100MB", 100 << 20, false},
		{"1G", 1 << 30, false},
		{"2GB", 2 << 30, false},
		{" 50mb ", 50 << 20, false},
		{"", 0, true},
		{"nope", 0, true},
		{"-5MB", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseSize(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseSize(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestGatewayCfg_MapModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		modelMap map[string]string
		input    string
		want     string
	}{
		{
			name:     "exact match",
			modelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"},
			input:    "claude-sonnet-4-6",
			want:     "gpt-4o",
		},
		{
			name:     "no match passthrough",
			modelMap: map[string]string{"claude-sonnet-4-6": "gpt-4o"},
			input:    "gpt-4o-mini",
			want:     "gpt-4o-mini",
		},
		{
			name:     "glob star prefix",
			modelMap: map[string]string{"claude-*": "gpt-4o"},
			input:    "claude-sonnet-4-6",
			want:     "gpt-4o",
		},
		{
			name:     "glob no match",
			modelMap: map[string]string{"claude-*": "gpt-4o"},
			input:    "gpt-4o-mini",
			want:     "gpt-4o-mini",
		},
		{
			name:     "glob question mark",
			modelMap: map[string]string{"gpt-?o": "mapped"},
			input:    "gpt-4o",
			want:     "mapped",
		},
		{
			name:     "glob bracket",
			modelMap: map[string]string{"gpt-[34]o": "mapped"},
			input:    "gpt-4o",
			want:     "mapped",
		},
		{
			name:     "catch-all star",
			modelMap: map[string]string{"*": "default-model"},
			input:    "anything",
			want:     "default-model",
		},
		{
			name:     "exact wins over glob",
			modelMap: map[string]string{"claude-sonnet-4-6": "exact", "claude-*": "glob"},
			input:    "claude-sonnet-4-6",
			want:     "exact",
		},
		{
			name:     "glob wins over catch-all",
			modelMap: map[string]string{"claude-*": "glob", "*": "default"},
			input:    "claude-sonnet-4-6",
			want:     "glob",
		},
		{
			name:     "longer glob wins over shorter",
			modelMap: map[string]string{"claude-sonnet-*": "specific", "claude-*": "general"},
			input:    "claude-sonnet-4-6",
			want:     "specific",
		},
		{
			name:     "catch-all when no glob matches",
			modelMap: map[string]string{"claude-*": "glob", "*": "default"},
			input:    "gpt-4o",
			want:     "default",
		},
		{
			name:     "empty map passthrough",
			modelMap: nil,
			input:    "claude-sonnet-4-6",
			want:     "claude-sonnet-4-6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := GatewayCfg{ModelMap: tt.modelMap}
			if got := cfg.MapModel(tt.input); got != tt.want {
				t.Errorf("MapModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGatewayCfg_MaxTokens(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	cfg := GatewayCfg{}
	if got := cfg.TimeoutDuration().Seconds(); got != 600 {
		t.Errorf("TimeoutDuration() = %f, want 600", got)
	}
	cfg.Timeout = "30s"
	if got := cfg.TimeoutDuration().Seconds(); got != 30 {
		t.Errorf("TimeoutDuration() = %f, want 30", got)
	}
}
