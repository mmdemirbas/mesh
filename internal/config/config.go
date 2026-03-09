package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level mesh configuration.
type Config struct {
	Name        string       `yaml:"name"`
	Proxies     []Proxy      `yaml:"proxies"`
	Relays      []Relay      `yaml:"relays"`
	Servers     []Server     `yaml:"servers"`
	Connections []Connection `yaml:"connections"`
	Log         LogCfg       `yaml:"log"`
}

// Proxy is a standalone proxy (works without any peer connection).
type Proxy struct {
	Type     string `yaml:"type"`               // "socks" or "http"
	Bind     string `yaml:"bind"`               // Listen address
	Upstream string `yaml:"upstream,omitempty"` // For HTTP proxy: optional SOCKS upstream
}

// Relay is a standalone TCP relay (e.g., replaces socat sidecar).
type Relay struct {
	Bind   string `yaml:"bind"`   // Where to listen locally
	Target string `yaml:"target"` // Where to connect to
}

// Server is an SSH server that accepts incoming connections.
type Server struct {
	Listen         string   `yaml:"listen"`
	HostKey        string   `yaml:"host_key"`
	AuthorizedKeys string   `yaml:"authorized_keys"`
	Shell          []string `yaml:"shell"` // Command to run for interactive sessions
}

// Connection is an outbound SSH connection to a peer or standard sshd.
type Connection struct {
	Name     string       `yaml:"name"`
	Targets  []string     `yaml:"targets"` // Tried in order (fallback)
	Retry    string       `yaml:"retry"`   // Duration string, e.g. "10s"
	Auth     AuthCfg      `yaml:"auth"`
	Forwards []ForwardSet `yaml:"forwards"`
}

// ForwardSet represents a distinct SSH connection for a group of port forwards and proxies.
type ForwardSet struct {
	Name    string    `yaml:"name"`
	Options Options   `yaml:"options"`
	Remote  []FwdRule `yaml:"remote"`
	Local   []FwdRule `yaml:"local"`
	Proxies PxyCfg    `yaml:"proxies"`
}

// Options allows tweaking specific SSH behavior for this forward set.
type Options struct {
	IPQoS string `yaml:"ipqos"` // "lowdelay", "throughput", etc.
}

// AuthCfg configures key-based authentication for a connection.
type AuthCfg struct {
	Key        string `yaml:"key"`
	KnownHosts string `yaml:"known_hosts"`
}

// FwdRule is a single port forwarding rule.
type FwdRule struct {
	Bind   string `yaml:"bind"`   // Where to listen
	Target string `yaml:"target"` // Where to connect
}

// PxyCfg holds connection-scoped proxies, explicitly separated by side.
type PxyCfg struct {
	Remote []Proxy `yaml:"remote"` // Bind on peer, exit through this side
	Local  []Proxy `yaml:"local"`  // Bind on this side, exit through peer
}

// LogCfg configures logging.
type LogCfg struct {
	Level string `yaml:"level"` // "debug", "info", "warn", "error"
}

// Load reads and parses a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expand ~ in all path fields
	for i := range cfg.Servers {
		cfg.Servers[i].HostKey = expandHome(cfg.Servers[i].HostKey)
		cfg.Servers[i].AuthorizedKeys = expandHome(cfg.Servers[i].AuthorizedKeys)
	}
	for i := range cfg.Connections {
		cfg.Connections[i].Auth.Key = expandHome(cfg.Connections[i].Auth.Key)
		cfg.Connections[i].Auth.KnownHosts = expandHome(cfg.Connections[i].Auth.KnownHosts)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Name == "" {
		c.Name = "mesh"
	}

	for i, s := range c.Servers {
		if s.Listen == "" {
			return fmt.Errorf("servers[%d]: listen is required", i)
		}
		if err := requireFile(s.HostKey, "host_key"); err != nil {
			return fmt.Errorf("servers[%d]: %w", i, err)
		}
		if err := requireFile(s.AuthorizedKeys, "authorized_keys"); err != nil {
			return fmt.Errorf("servers[%d]: %w", i, err)
		}
	}

	for i, conn := range c.Connections {
		if len(conn.Targets) == 0 {
			return fmt.Errorf("connections[%d] %q: targets is required", i, conn.Name)
		}
		if err := requireFile(conn.Auth.Key, "auth.key"); err != nil {
			return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
		}
		if err := requireFile(conn.Auth.KnownHosts, "auth.known_hosts"); err != nil {
			return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
		}
		for j, fset := range conn.Forwards {
			// Validate connection proxies
			if err := validateProxies(fset.Proxies.Remote); err != nil {
				return fmt.Errorf("connections[%d] %q forwards[%d] %q remote proxies: %w", i, conn.Name, j, fset.Name, err)
			}
			if err := validateProxies(fset.Proxies.Local); err != nil {
				return fmt.Errorf("connections[%d] %q forwards[%d] %q local proxies: %w", i, conn.Name, j, fset.Name, err)
			}
		}
	}

	// Validate standalone relays
	for i, r := range c.Relays {
		if r.Bind == "" {
			return fmt.Errorf("relay[%d]: bind is required", i)
		}
		if r.Target == "" {
			return fmt.Errorf("relay[%d]: target is required", i)
		}
	}

	// Validate standalone proxies
	if err := validateProxies(c.Proxies); err != nil {
		return fmt.Errorf("standalone proxies: %w", err)
	}

	return nil
}

func validateProxies(proxies []Proxy) error {
	for i, p := range proxies {
		if p.Bind == "" {
			return fmt.Errorf("[%d]: bind is required", i)
		}
		if p.Type != "socks" && p.Type != "http" {
			return fmt.Errorf("[%d]: type must be 'socks' or 'http'", i)
		}
	}
	return nil
}

func requireFile(path, field string) error {
	if path == "" {
		return fmt.Errorf("%s is required", field)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s file inaccessible: %w", field, err)
	}
	return nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
