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
	if len(cfg.LANDiscoveryGroup) != 0 {
		t.Errorf("LANDiscoveryGroup = %v, want empty (disabled by default)", cfg.LANDiscoveryGroup)
	}
}

func TestClipsyncCfgUnmarshalYAMLWithGroups(t *testing.T) {
	input := []byte(`
bind: "0.0.0.0:7755"
lan_discovery_group: ["team-alpha", "team-beta"]
static_peers: ["10.0.0.1:7755"]
`)

	var cfg ClipsyncCfg
	if err := yamlUnmarshal(input, &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(cfg.LANDiscoveryGroup) != 2 || cfg.LANDiscoveryGroup[0] != "team-alpha" {
		t.Errorf("LANDiscoveryGroup = %v, want [team-alpha, team-beta]", cfg.LANDiscoveryGroup)
	}
	if len(cfg.StaticPeers) != 1 || cfg.StaticPeers[0] != "10.0.0.1:7755" {
		t.Errorf("StaticPeers = %v, want [10.0.0.1:7755]", cfg.StaticPeers)
	}
}

func TestUnmarshalConfigsSkipsExtensionKeys(t *testing.T) {
	input := []byte(`
x-ignore-global: &ignore_global
  - ".DS_Store"
  - "*.tmp"

mynode:
  log_level: debug
  filesync:
    - folders:
        "/data":
          ignore: *ignore_global
`)
	cfgs, err := unmarshalConfigs(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfgs["x-ignore-global"]; ok {
		t.Error("extension key x-ignore-global should be skipped")
	}
	cfg, ok := cfgs["mynode"]
	if !ok {
		t.Fatal("expected mynode in config")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
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
	fsCfg := FilesyncCfg{
		Bind:          "0.0.0.0:7756",
		MaxConcurrent: 0,
		Peers:         map[string][]string{"peer1": {"192.168.1.10:7756"}},
		Folders: map[string]FolderCfgRaw{
			"docs": {Path: t.TempDir()},
		},
		Defaults: FilesyncDefaults{Peers: []string{"peer1"}},
	}
	_ = fsCfg.Resolve()
	cfg := &Config{Filesync: []FilesyncCfg{fsCfg}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for max_concurrent=0")
	}
	if !strings.Contains(err.Error(), "max_concurrent must be positive") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_FilesyncDisabledNoPeers(t *testing.T) {
	fsCfg := FilesyncCfg{
		Bind:          "0.0.0.0:7756",
		MaxConcurrent: 4,
		Folders: map[string]FolderCfgRaw{
			"archive": {Path: t.TempDir(), Direction: "disabled"},
		},
	}
	_ = fsCfg.Resolve()
	cfg := &Config{Filesync: []FilesyncCfg{fsCfg}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("disabled folder without peers should be valid: %v", err)
	}
}

func TestValidate_FilesyncDryRunRequiresPeers(t *testing.T) {
	fsCfg := FilesyncCfg{
		Bind:          "0.0.0.0:7756",
		MaxConcurrent: 4,
		Folders: map[string]FolderCfgRaw{
			"code": {Path: t.TempDir(), Direction: "dry-run"},
		},
	}
	_ = fsCfg.Resolve()
	cfg := &Config{Filesync: []FilesyncCfg{fsCfg}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("dry-run folder without peers should fail validation")
	}
	if !strings.Contains(err.Error(), "at least one peer") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFilesyncResolve(t *testing.T) {
	tests := []struct {
		name    string
		cfg     FilesyncCfg
		wantErr string
		check   func(t *testing.T, cfg *FilesyncCfg)
	}{
		{
			name: "basic resolution with defaults",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{
					Peers:          []string{"hw"},
					Direction:      "send-only",
					IgnorePatterns: []string{"*.tmp"},
				},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				if len(cfg.ResolvedFolders) != 1 {
					t.Fatalf("resolved %d folders, want 1", len(cfg.ResolvedFolders))
				}
				f := cfg.ResolvedFolders[0]
				if f.ID != "code" {
					t.Errorf("ID = %q, want code", f.ID)
				}
				if f.Direction != "send-only" {
					t.Errorf("Direction = %q, want send-only", f.Direction)
				}
				if len(f.Peers) != 1 || f.Peers[0] != "10.0.0.1:7756" {
					t.Errorf("Peers = %v, want [10.0.0.1:7756]", f.Peers)
				}
				if len(f.IgnorePatterns) != 1 || f.IgnorePatterns[0] != "*.tmp" {
					t.Errorf("IgnorePatterns = %v, want [*.tmp]", f.IgnorePatterns)
				}
			},
		},
		{
			name: "folder overrides peers and direction",
			cfg: FilesyncCfg{
				Bind: "0.0.0.0:7756",
				Peers: map[string][]string{
					"hw":  {"10.0.0.1:7756"},
					"mbp": {"10.0.0.2:7756"},
				},
				Defaults: FilesyncDefaults{
					Peers:     []string{"hw"},
					Direction: "send-only",
				},
				Folders: map[string]FolderCfgRaw{
					"docs": {
						Path:      "/tmp/docs",
						Peers:     []string{"hw", "mbp"},
						Direction: "receive-only",
					},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				f := cfg.ResolvedFolders[0]
				if f.Direction != "receive-only" {
					t.Errorf("Direction = %q, want receive-only", f.Direction)
				}
				if len(f.Peers) != 2 {
					t.Errorf("Peers count = %d, want 2", len(f.Peers))
				}
			},
		},
		{
			name: "ignore patterns extend defaults",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{
					Peers:          []string{"hw"},
					IgnorePatterns: []string{".DS_Store", "*.tmp"},
				},
				Folders: map[string]FolderCfgRaw{
					"code": {
						Path:           "/tmp/code",
						IgnorePatterns: []string{"**/target"},
					},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				f := cfg.ResolvedFolders[0]
				want := []string{".DS_Store", "*.tmp", "**/target"}
				if len(f.IgnorePatterns) != len(want) {
					t.Fatalf("IgnorePatterns = %v, want %v", f.IgnorePatterns, want)
				}
				for i, p := range want {
					if f.IgnorePatterns[i] != p {
						t.Errorf("IgnorePatterns[%d] = %q, want %q", i, f.IgnorePatterns[i], p)
					}
				}
			},
		},
		{
			name: "direction defaults to send-receive",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{
					Peers: []string{"hw"},
				},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				if cfg.ResolvedFolders[0].Direction != "send-receive" {
					t.Errorf("Direction = %q, want send-receive", cfg.ResolvedFolders[0].Direction)
				}
			},
		},
		{
			name: "multi-address peer expands",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756", "10.0.0.2:7756"}},
				Defaults: FilesyncDefaults{
					Peers: []string{"hw"},
				},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				if len(cfg.ResolvedFolders[0].Peers) != 2 {
					t.Errorf("Peers = %v, want 2 addresses", cfg.ResolvedFolders[0].Peers)
				}
			},
		},
		{
			name: "unknown peer name",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{
					Peers: []string{"unknown"},
				},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code"},
				},
			},
			wantErr: `unknown peer "unknown"`,
		},
		{
			name: "folder-level unknown peer name",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code", Peers: []string{"missing"}},
				},
			},
			wantErr: `unknown peer "missing"`,
		},
		{
			name: "dry-run direction resolves",
			cfg: FilesyncCfg{
				Bind:  "0.0.0.0:7756",
				Peers: map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{
					Peers:     []string{"hw"},
					Direction: "dry-run",
				},
				Folders: map[string]FolderCfgRaw{
					"code": {Path: "/tmp/code"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				if cfg.ResolvedFolders[0].Direction != "dry-run" {
					t.Errorf("Direction = %q, want dry-run", cfg.ResolvedFolders[0].Direction)
				}
			},
		},
		{
			name: "disabled direction without peers",
			cfg: FilesyncCfg{
				Bind: "0.0.0.0:7756",
				Folders: map[string]FolderCfgRaw{
					"archive": {Path: "/tmp/archive", Direction: "disabled"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				f := cfg.ResolvedFolders[0]
				if f.Direction != "disabled" {
					t.Errorf("Direction = %q, want disabled", f.Direction)
				}
				if len(f.Peers) != 0 {
					t.Errorf("Peers = %v, want empty for disabled", f.Peers)
				}
			},
		},
		{
			name: "sorted by ID",
			cfg: FilesyncCfg{
				Bind:     "0.0.0.0:7756",
				Peers:    map[string][]string{"hw": {"10.0.0.1:7756"}},
				Defaults: FilesyncDefaults{Peers: []string{"hw"}},
				Folders: map[string]FolderCfgRaw{
					"zebra":  {Path: "/tmp/z"},
					"alpha":  {Path: "/tmp/a"},
					"middle": {Path: "/tmp/m"},
				},
			},
			check: func(t *testing.T, cfg *FilesyncCfg) {
				ids := make([]string, len(cfg.ResolvedFolders))
				for i, f := range cfg.ResolvedFolders {
					ids[i] = f.ID
				}
				if ids[0] != "alpha" || ids[1] != "middle" || ids[2] != "zebra" {
					t.Errorf("IDs = %v, want [alpha middle zebra]", ids)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Resolve()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, &tt.cfg)
			}
		})
	}
}

func TestFilesyncYAMLParsing(t *testing.T) {
	input := `
bind: "0.0.0.0:7756"
scan_interval: "3600s"
peers:
  hw: ["10.0.0.1:7756"]
  mbp: ["10.0.0.2:7756", "10.0.0.3:7756"]
defaults:
  peers: ["hw"]
  direction: "send-receive"
  ignore_patterns: [".DS_Store"]
folders:
  code:
    path: "/tmp/code"
  docs:
    path: "/tmp/docs"
    peers: ["hw", "mbp"]
    direction: "send-only"
    ignore_patterns: ["*.pdf"]
`
	var cfg FilesyncCfg
	if err := yamlUnmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if cfg.Bind != "0.0.0.0:7756" {
		t.Errorf("Bind = %q", cfg.Bind)
	}
	if cfg.ScanInterval != "3600s" {
		t.Errorf("ScanInterval = %q", cfg.ScanInterval)
	}
	if len(cfg.Peers) != 2 {
		t.Errorf("Peers count = %d, want 2", len(cfg.Peers))
	}
	if len(cfg.Folders) != 2 {
		t.Errorf("Folders count = %d, want 2", len(cfg.Folders))
	}

	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve error: %v", err)
	}

	if len(cfg.ResolvedFolders) != 2 {
		t.Fatalf("ResolvedFolders count = %d, want 2", len(cfg.ResolvedFolders))
	}

	// Sorted: code < docs
	code := cfg.ResolvedFolders[0]
	docs := cfg.ResolvedFolders[1]

	if code.ID != "code" || docs.ID != "docs" {
		t.Fatalf("IDs = [%q, %q], want [code, docs]", code.ID, docs.ID)
	}

	// code inherits defaults
	if len(code.Peers) != 1 || code.Peers[0] != "10.0.0.1:7756" {
		t.Errorf("code.Peers = %v", code.Peers)
	}
	if code.Direction != "send-receive" {
		t.Errorf("code.Direction = %q", code.Direction)
	}
	if len(code.IgnorePatterns) != 1 || code.IgnorePatterns[0] != ".DS_Store" {
		t.Errorf("code.IgnorePatterns = %v", code.IgnorePatterns)
	}

	// docs overrides peers and direction, extends ignore patterns
	if len(docs.Peers) != 3 {
		t.Errorf("docs.Peers = %v, want 3 addresses (hw=1 + mbp=2)", docs.Peers)
	}
	if docs.Direction != "send-only" {
		t.Errorf("docs.Direction = %q", docs.Direction)
	}
	if len(docs.IgnorePatterns) != 2 {
		t.Errorf("docs.IgnorePatterns = %v, want [.DS_Store, *.pdf]", docs.IgnorePatterns)
	}
}

func TestParseBandwidth(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"10MB", 10_000_000, false},
		{"500KB", 500_000, false},
		{"1GB", 1_000_000_000, false},
		{"100", 100, false},
		{"  50 MB  ", 50_000_000, false},
		{"0MB", 0, true},
		{"-5MB", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseBandwidth(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBandwidth(%q) error=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseBandwidth(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
