package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Listen         string            `yaml:"listen"`
	HostKey        string            `yaml:"host_key"`
	AuthorizedKeys string            `yaml:"authorized_keys"`
	Shell          []string          `yaml:"shell"` // Command to run for interactive sessions
	Options        map[string]string `yaml:"options"`
}

// Connection is an outbound SSH connection to a peer or standard sshd.
type Connection struct {
	Name     string            `yaml:"name"`
	Targets  []string          `yaml:"targets"` // Tried in order (fallback)
	Retry    string            `yaml:"retry"`   // Duration string, e.g. "10s"
	Auth     AuthCfg           `yaml:"auth"`
	Options  map[string]string `yaml:"options"`
	Forwards []ForwardSet      `yaml:"forwards"`
}

// ForwardSet represents a distinct SSH connection for a group of port forwards and proxies.
type ForwardSet struct {
	Name    string            `yaml:"name"`
	Options map[string]string `yaml:"options"`
	Remote  []FwdRule         `yaml:"remote"`
	Local   []FwdRule         `yaml:"local"`
	Proxies PxyCfg            `yaml:"proxies"`
}

// GetOption retrieves a configuration value by key, case-insensitively.
func GetOption(options map[string]string, key string) string {
	for k, v := range options {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
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
		if conn.Retry != "" {
			if _, err := time.ParseDuration(conn.Retry); err != nil {
				return fmt.Errorf("connections[%d] %q: invalid retry duration %q: %w", i, conn.Name, conn.Retry, err)
			}
		}
		if err := requireFile(conn.Auth.Key, "auth.key"); err != nil {
			return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
		}
		if err := requireFile(conn.Auth.KnownHosts, "auth.known_hosts"); err != nil {
			return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
		}
		for j, fset := range conn.Forwards {
			if err := validateIPQoS(GetOption(fset.Options, "IPQoS")); err != nil {
				return fmt.Errorf("connections[%d] %q forwards[%d] %q: %w", i, conn.Name, j, fset.Name, err)
			}
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

	// Check for duplicate bind addresses across all components
	if err := c.checkDuplicateBinds(); err != nil {
		return err
	}

	return nil
}

// checkDuplicateBinds detects bind address collisions across all listeners.
// It normalizes common aliases (localhost → 127.0.0.1) and detects wildcard
// overlaps (0.0.0.0 or :: conflicts with any specific address on the same port).
func (c *Config) checkDuplicateBinds() error {
	seen := make(map[string]string)      // normalized addr -> description
	wildPorts := make(map[string]string) // port -> description (for wildcard binds)

	check := func(bind, desc string) error {
		if bind == "" {
			return nil
		}
		normalized := normalizeBind(bind)
		host, port, err := splitHostPort(normalized)
		if err != nil {
			return nil // skip unparsable; will fail at runtime anyway
		}

		// Check exact duplicate
		if prev, ok := seen[normalized]; ok {
			return fmt.Errorf("duplicate bind address %q: used by %s and %s", bind, prev, desc)
		}
		seen[normalized] = desc

		// Check wildcard overlap: if this is a wildcard, check if any specific addr uses the same port
		isWild := host == "0.0.0.0" || host == "::" || host == ""
		if isWild {
			if prev, ok := wildPorts[port]; ok {
				return fmt.Errorf("duplicate bind port %s: wildcard used by %s and %s", port, prev, desc)
			}
			wildPorts[port] = desc
			// Check against all existing specific-address entries on this port
			for addr, prev := range seen {
				_, p, _ := splitHostPort(addr)
				if p == port && addr != normalized {
					return fmt.Errorf("bind address %q (%s) conflicts with wildcard %q (%s) on port %s", addr, prev, bind, desc, port)
				}
			}
		} else if prev, ok := wildPorts[port]; ok {
			// A specific address conflicts with an existing wildcard on the same port
			return fmt.Errorf("bind address %q (%s) conflicts with wildcard (%s) on port %s", bind, desc, prev, port)
		}

		return nil
	}
	for i, p := range c.Proxies {
		if err := check(p.Bind, fmt.Sprintf("proxies[%d]", i)); err != nil {
			return err
		}
	}
	for i, r := range c.Relays {
		if err := check(r.Bind, fmt.Sprintf("relays[%d]", i)); err != nil {
			return err
		}
	}
	for i, s := range c.Servers {
		if err := check(s.Listen, fmt.Sprintf("servers[%d]", i)); err != nil {
			return err
		}
	}
	for i, conn := range c.Connections {
		for j, fset := range conn.Forwards {
			for k, fwd := range fset.Local {
				if err := check(fwd.Bind, fmt.Sprintf("connections[%d].forwards[%d].local[%d]", i, j, k)); err != nil {
					return err
				}
			}
			for k, pxy := range fset.Proxies.Local {
				if err := check(pxy.Bind, fmt.Sprintf("connections[%d].forwards[%d].proxies.local[%d]", i, j, k)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// normalizeBind canonicalizes a bind address for comparison.
func normalizeBind(addr string) string {
	addr = strings.Replace(addr, "localhost", "127.0.0.1", 1)
	return addr
}

// splitHostPort wraps net.SplitHostPort, handling addresses without an explicit host.
func splitHostPort(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	return
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

// validIPQoSValues is the set of recognized IPQoS names (OpenSSH-compatible).
var validIPQoSValues = map[string]bool{
	"lowdelay": true, "throughput": true, "reliability": true, "none": true,
	"af11": true, "af12": true, "af13": true,
	"af21": true, "af22": true, "af23": true,
	"af31": true, "af32": true, "af33": true,
	"af41": true, "af42": true, "af43": true,
	"ef":  true,
	"cs0": true, "cs1": true, "cs2": true, "cs3": true,
	"cs4": true, "cs5": true, "cs6": true, "cs7": true,
}

func validateIPQoS(value string) error {
	if value == "" {
		return nil
	}
	if !validIPQoSValues[strings.ToLower(value)] {
		return fmt.Errorf("unknown ipqos value %q", value)
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
