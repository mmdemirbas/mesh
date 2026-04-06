package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	tests := []struct {
		input, want string
	}{
		{"~/foo/bar", filepath.Join(home, "foo/bar")},
		{"~/.ssh/id_rsa", filepath.Join(home, ".ssh/id_rsa")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~", "~"},                   // no slash after ~, not expanded
		{"~user/path", "~user/path"}, // not current user, not expanded
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := expandHome(tt.input)
			if got != tt.want {
				t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRequireFile(t *testing.T) {
	// Empty path
	if err := requireFile("", "test_key"); err == nil {
		t.Error("expected error for empty path")
	}

	// Non-existent file
	if err := requireFile("/nonexistent/path/file", "test_key"); err == nil {
		t.Error("expected error for non-existent file")
	}

	// Existing file
	f := filepath.Join(t.TempDir(), "exists.txt")
	_ = os.WriteFile(f, []byte("ok"), 0600)
	if err := requireFile(f, "test_key"); err != nil {
		t.Errorf("expected no error for existing file, got: %v", err)
	}
}

func TestLoadUnvalidated(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "mesh.yml")
	content := `
mynode:
  listeners:
    - type: socks
      bind: "127.0.0.1:1080"
    - type: http
      bind: "127.0.0.1:3128"
  connections:
    - name: remote
      targets: ["user@10.0.0.1:22"]
      retry: "10s"
      auth:
        key: "~/.ssh/id_rsa"
        known_hosts: "~/.ssh/known_hosts"
      forwards:
        - name: web
          local:
            - type: forward
              bind: "127.0.0.1:8080"
              target: "10.0.0.1:80"
`
	_ = os.WriteFile(cfgFile, []byte(content), 0600)

	cfgs, err := LoadUnvalidated(cfgFile)
	if err != nil {
		t.Fatalf("LoadUnvalidated failed: %v", err)
	}

	cfg, ok := cfgs["mynode"]
	if !ok {
		t.Fatal("mynode not found in config")
	}

	if len(cfg.Listeners) != 2 {
		t.Errorf("listeners count = %d, want 2", len(cfg.Listeners))
	}
	if cfg.Listeners[0].Type != "socks" {
		t.Errorf("listener[0].Type = %q, want socks", cfg.Listeners[0].Type)
	}
	if len(cfg.Connections) != 1 {
		t.Fatalf("connections count = %d, want 1", len(cfg.Connections))
	}
	if cfg.Connections[0].Name != "remote" {
		t.Errorf("connection name = %q, want remote", cfg.Connections[0].Name)
	}
	if len(cfg.Connections[0].Forwards) != 1 {
		t.Fatalf("forwards count = %d, want 1", len(cfg.Connections[0].Forwards))
	}
	if len(cfg.Connections[0].Forwards[0].Local) != 1 {
		t.Fatalf("local forwards count = %d, want 1", len(cfg.Connections[0].Forwards[0].Local))
	}
}

func TestLoadUnvalidated_NonExistentFile(t *testing.T) {
	_, err := LoadUnvalidated("/nonexistent/mesh.yml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoadUnvalidated_InvalidYAML(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.yml")
	_ = os.WriteFile(f, []byte("{{invalid yaml"), 0600)

	_, err := LoadUnvalidated(f)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoadUnvalidated_EnvExpansion(t *testing.T) {
	t.Setenv("MESH_TEST_PORT", "9999")

	f := filepath.Join(t.TempDir(), "env.yml")
	content := `
test:
  listeners:
    - type: socks
      bind: "127.0.0.1:${MESH_TEST_PORT}"
`
	_ = os.WriteFile(f, []byte(content), 0600)

	cfgs, err := LoadUnvalidated(f)
	if err != nil {
		t.Fatalf("LoadUnvalidated failed: %v", err)
	}

	cfg := cfgs["test"]
	if cfg.Listeners[0].Bind != "127.0.0.1:9999" {
		t.Errorf("bind = %q, want 127.0.0.1:9999 (env not expanded)", cfg.Listeners[0].Bind)
	}
}

func TestLoad_ServiceNotFound(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mesh.yml")
	_ = os.WriteFile(f, []byte("mynode:\n  listeners: []\n"), 0600)

	_, err := Load(f, "nonexistent")
	if err == nil {
		t.Error("expected error for missing service")
	}
}

func TestValidate_ListenerMissingBind(t *testing.T) {
	cfg := &Config{
		Listeners: []Listener{{Type: "socks"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing bind")
	}
}

func TestValidate_ListenerMissingType(t *testing.T) {
	cfg := &Config{
		Listeners: []Listener{{Bind: "127.0.0.1:1080"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing type")
	}
}

func TestValidate_ListenerUnknownType(t *testing.T) {
	cfg := &Config{
		Listeners: []Listener{{Bind: "127.0.0.1:1080", Type: "tcp"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestValidate_RelayMissingTarget(t *testing.T) {
	cfg := &Config{
		Listeners: []Listener{{Bind: "127.0.0.1:1080", Type: "relay"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for relay missing target")
	}
}

func TestValidate_ConnectionMissingTargets(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{Name: "test"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing targets")
	}
}

func TestValidate_InvalidRetryDuration(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(f, []byte("key"), 0600)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(kh, []byte("host"), 0600)

	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Targets: []string{"host:22"},
			Retry:   "not-a-duration",
			Auth:    AuthCfg{Key: f, KnownHosts: kh},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid retry duration")
	}
}

func TestWarnUnsupportedOptions(t *testing.T) {
	// Should not panic with supported and unsupported options
	cfg := &Config{
		Listeners: []Listener{{
			Bind: "127.0.0.1:22",
			Options: map[string]string{
				"Ciphers":       "aes256-ctr",
				"UnsupportedOp": "value",
			},
		}},
		Connections: []Connection{{
			Name:    "test",
			Options: map[string]string{"MACs": "hmac-sha2-256"},
			Forwards: []ForwardSet{{
				Name:    "fwd",
				Options: map[string]string{"UnknownOption": "x"},
			}},
		}},
	}
	WarnUnsupportedOptions(cfg) // should not panic
}

func TestValidate_ConnectionNoAuth(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Targets: []string{"host:22"},
			Auth:    AuthCfg{},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error when no auth methods configured")
	}
}

func TestValidate_ConnectionAgentAuth(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Targets: []string{"host:22"},
			Auth:    AuthCfg{Agent: true},
		}},
	}
	// Should not fail on missing key file — agent is sufficient
	err := cfg.Validate()
	if err != nil {
		t.Errorf("validate with agent auth should not fail: %v", err)
	}
}

func TestValidate_ConnectionPasswordCommandAuth(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Targets: []string{"host:22"},
			Auth:    AuthCfg{PasswordCommand: "echo secret"},
		}},
	}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("validate with password_command should not fail: %v", err)
	}
}

func TestValidate_InvalidMode(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(f, []byte("key"), 0600)

	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Mode:    "invalid",
			Targets: []string{"host:22"},
			Auth:    AuthCfg{Key: f},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestValidate_MultiplexMode(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Mode:    "multiplex",
			Targets: []string{"host1:22", "host2:22"},
			Auth:    AuthCfg{PasswordCommand: "echo pass"},
		}},
	}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("validate multiplex mode should not fail: %v", err)
	}
}

func TestValidate_FailoverModeExplicit(t *testing.T) {
	cfg := &Config{
		Connections: []Connection{{
			Name:    "test",
			Mode:    "failover",
			Targets: []string{"host:22"},
			Auth:    AuthCfg{Agent: true},
		}},
	}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("validate explicit failover mode should not fail: %v", err)
	}
}

func TestValidate_DuplicateNames(t *testing.T) {
	validConn := func(name string) Connection {
		return Connection{
			Name:    name,
			Targets: []string{"host:22"},
			Auth:    AuthCfg{Agent: true},
		}
	}

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "duplicate connection names",
			cfg: Config{
				Connections: []Connection{validConn("vpn"), validConn("vpn")},
			},
			wantErr: `duplicate connection name "vpn": connections[0] and connections[1]`,
		},
		{
			name: "duplicate forward set names",
			cfg: Config{
				Connections: []Connection{{
					Name:    "conn",
					Targets: []string{"host:22"},
					Auth:    AuthCfg{Agent: true},
					Forwards: []ForwardSet{
						{Name: "fwd", Local: []Forward{{Type: "forward", Bind: "127.0.0.1:8080", Target: "127.0.0.1:80"}}},
						{Name: "fwd", Local: []Forward{{Type: "forward", Bind: "127.0.0.1:9090", Target: "127.0.0.1:90"}}},
					},
				}},
			},
			wantErr: `duplicate forward set name "fwd": forwards[0] and forwards[1]`,
		},
		{
			name: "duplicate listener names",
			cfg: Config{
				Listeners: []Listener{
					{Name: "proxy", Type: "socks", Bind: "127.0.0.1:1080"},
					{Name: "proxy", Type: "http", Bind: "127.0.0.1:1081"},
				},
			},
			wantErr: `duplicate listener name "proxy": listeners[0] and listeners[1]`,
		},
		{
			name: "unique names pass",
			cfg: Config{
				Connections: []Connection{validConn("vpn"), validConn("office")},
				Listeners: []Listener{
					{Name: "socks", Type: "socks", Bind: "127.0.0.1:1080"},
					{Name: "http", Type: "http", Bind: "127.0.0.1:1081"},
				},
			},
			wantErr: "",
		},
		{
			name: "empty listener names are not checked",
			cfg: Config{
				Listeners: []Listener{
					{Type: "socks", Bind: "127.0.0.1:1080"},
					{Type: "socks", Bind: "127.0.0.1:1081"},
				},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
			}
		})
	}
}

func TestValidate_FilesyncMaxConcurrentZero(t *testing.T) {
	cfg := &Config{
		Filesync: []FilesyncCfg{{
			Bind:          "0.0.0.0:7756",
			MaxConcurrent: 0,
			Folders: []FolderCfg{{
				ID:        "docs",
				Path:      t.TempDir(),
				Peers:     []string{"192.168.1.10:7756"},
				Direction: "send-receive",
			}},
		}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for max_concurrent=0")
	}
	if !strings.Contains(err.Error(), "max_concurrent must be positive") {
		t.Errorf("unexpected error: %v", err)
	}
}
