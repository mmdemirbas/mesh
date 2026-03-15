package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGetOption(t *testing.T) {
	opts := map[string]string{
		"Ciphers":        "aes256-ctr",
		"ConnectTimeout": "10",
		"IPQoS":          "lowdelay",
	}

	tests := []struct {
		key, want string
	}{
		{"Ciphers", "aes256-ctr"},
		{"ciphers", "aes256-ctr"}, // case insensitive
		{"CIPHERS", "aes256-ctr"}, // all caps
		{"ConnectTimeout", "10"},  // exact match
		{"connecttimeout", "10"},  // lowercase
		{"IPQoS", "lowdelay"},     // mixed case key
		{"ipqos", "lowdelay"},     // lowercase
		{"NonExistent", ""},       // missing key
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := GetOption(opts, tt.key)
			if got != tt.want {
				t.Errorf("GetOption(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestGetOptionNilMap(t *testing.T) {
	got := GetOption(nil, "anything")
	if got != "" {
		t.Errorf("GetOption(nil, ...) = %q, want empty", got)
	}
}

func TestNormalizeBind(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"localhost:8080", "127.0.0.1:8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"0.0.0.0:22", "0.0.0.0:22"},
		{"192.168.1.1:443", "192.168.1.1:443"},
		{"localhost:22", "127.0.0.1:22"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeBind(tt.input)
			if got != tt.want {
				t.Errorf("normalizeBind(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateForwards(t *testing.T) {
	tests := []struct {
		name    string
		fwds    []Forward
		wantErr bool
	}{
		{
			"valid forward",
			[]Forward{{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"}},
			false,
		},
		{
			"valid socks",
			[]Forward{{Type: "socks", Bind: "127.0.0.1:1080"}},
			false,
		},
		{
			"valid http",
			[]Forward{{Type: "http", Bind: "127.0.0.1:3128"}},
			false,
		},
		{
			"missing bind",
			[]Forward{{Type: "forward", Target: "10.0.0.1:80"}},
			true,
		},
		{
			"forward without target",
			[]Forward{{Type: "forward", Bind: "127.0.0.1:8080"}},
			true,
		},
		{
			"invalid type",
			[]Forward{{Type: "tcp", Bind: "127.0.0.1:8080"}},
			true,
		},
		{
			"empty type",
			[]Forward{{Type: "", Bind: "127.0.0.1:8080"}},
			true,
		},
		{
			"empty slice",
			[]Forward{},
			false,
		},
		{
			"multiple valid",
			[]Forward{
				{Type: "forward", Bind: "127.0.0.1:8080", Target: "10.0.0.1:80"},
				{Type: "socks", Bind: "127.0.0.1:1080"},
			},
			false,
		},
		{
			"socks with optional target",
			[]Forward{{Type: "socks", Bind: "127.0.0.1:1080", Target: "upstream:1080"}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForwards(tt.fwds)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateForwards() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateIPQoS(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"", false},
		{"lowdelay", false},
		{"throughput", false},
		{"reliability", false},
		{"none", false},
		{"af11", false},
		{"af42", false},
		{"ef", false},
		{"cs0", false},
		{"cs7", false},
		{"lowdelay throughput", false}, // two values
		{"af11 ef", false},             // two DSCP values
		{"LOWDELAY", false},            // case insensitive
		{"LowDelay Throughput", false}, // mixed case
		{"invalid", true},              // unknown value
		{"lowdelay invalid", true},     // one valid, one invalid
		{"a b c", true},                // too many values
		{"lowdelay throughput extra", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			err := validateIPQoS(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateIPQoS(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestCheckDuplicateBinds(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			"no listeners",
			Config{},
			false,
		},
		{
			"unique binds",
			Config{Listeners: []Listener{
				{Bind: "127.0.0.1:8080"},
				{Bind: "127.0.0.1:9090"},
			}},
			false,
		},
		{
			"duplicate binds",
			Config{Listeners: []Listener{
				{Bind: "127.0.0.1:8080"},
				{Bind: "127.0.0.1:8080"},
			}},
			true,
		},
		{
			"localhost normalized to 127.0.0.1",
			Config{Listeners: []Listener{
				{Bind: "localhost:8080"},
				{Bind: "127.0.0.1:8080"},
			}},
			true,
		},
		{
			"wildcard conflicts with specific",
			Config{Listeners: []Listener{
				{Bind: "0.0.0.0:8080"},
				{Bind: "127.0.0.1:8080"},
			}},
			true,
		},
		{
			"different ports no conflict",
			Config{Listeners: []Listener{
				{Bind: "0.0.0.0:8080"},
				{Bind: "0.0.0.0:9090"},
			}},
			false,
		},
		{
			"forward bind conflicts with listener",
			Config{
				Listeners: []Listener{{Bind: "127.0.0.1:8080"}},
				Connections: []Connection{{
					Forwards: []ForwardSet{{
						Local: []Forward{{Bind: "127.0.0.1:8080"}},
					}},
				}},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.checkDuplicateBinds()
			if (err != nil) != tt.wantErr {
				t.Errorf("checkDuplicateBinds() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClipsyncCfgUnmarshalYAML(t *testing.T) {
	input := []byte(`bind: "0.0.0.0:7755"`)

	var cfg ClipsyncCfg
	if err := yamlUnmarshal(input, &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if cfg.Bind != "0.0.0.0:7755" {
		t.Errorf("Bind = %q, want %q", cfg.Bind, "0.0.0.0:7755")
	}
	if !cfg.LANDiscovery {
		t.Error("LANDiscovery should default to true")
	}
	if len(cfg.AllowSendTo) != 1 || cfg.AllowSendTo[0] != "all" {
		t.Errorf("AllowSendTo = %v, want [all]", cfg.AllowSendTo)
	}
	if len(cfg.AllowReceive) != 1 || cfg.AllowReceive[0] != "all" {
		t.Errorf("AllowReceive = %v, want [all]", cfg.AllowReceive)
	}
}

func TestClipsyncCfgUnmarshalYAMLOverride(t *testing.T) {
	input := []byte(`
bind: "0.0.0.0:7755"
lan_discovery: false
allow_send_to: ["none"]
allow_receive: ["192.168.1.1"]
`)

	var cfg ClipsyncCfg
	if err := yamlUnmarshal(input, &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if cfg.LANDiscovery {
		t.Error("LANDiscovery should be false when overridden")
	}
	if len(cfg.AllowSendTo) != 1 || cfg.AllowSendTo[0] != "none" {
		t.Errorf("AllowSendTo = %v, want [none]", cfg.AllowSendTo)
	}
	if len(cfg.AllowReceive) != 1 || cfg.AllowReceive[0] != "192.168.1.1" {
		t.Errorf("AllowReceive = %v, want [192.168.1.1]", cfg.AllowReceive)
	}
}

// yamlUnmarshal is a test helper that uses the same YAML library as production.
func yamlUnmarshal(data []byte, v interface{}) error {
	return yaml.Unmarshal(data, v)
}
