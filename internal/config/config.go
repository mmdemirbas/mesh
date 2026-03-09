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

// Config is the top-level mesh configuration schema.
// It defines local listeners and outbound connections to other mesh nodes or SSH servers.
type Config struct {
	// A friendly name for this mesh instance. Defaults to "mesh".
	Name string `yaml:"name,omitempty"`
	// Local server ports to bind (e.g., SOCKS, HTTP proxies, Relays, or an embedded SSH server).
	Listeners []Listener `yaml:"listeners,omitempty"`
	// Outbound SSH connections to other peers, which encapsulate port forwards and proxy rules.
	Connections []Connection `yaml:"connections,omitempty"`
	// Logging configuration.
	Log LogCfg `yaml:"log,omitempty"`
}

// Listener represents a local server port (proxy, relay, or sshd).
type Listener struct {
	// Optional friendly name for this listener.
	Name string `yaml:"name,omitempty"`
	// The type of listener to create. Can be "socks", "http", "relay", or "sshd".
	Type string `yaml:"type" jsonschema:"enum=socks,enum=http,enum=relay,enum=sshd"`
	// Local listening address (e.g., "127.0.0.1:1080" or "0.0.0.0:2222").
	Bind string `yaml:"bind"`
	// The destination address. Required for "relay", optional for "http" (forces it to act as a tunnel to a specific proxy).
	Target string `yaml:"target,omitempty"`
	// Path to the private host key. Required when type="sshd".
	HostKey string `yaml:"host_key,omitempty"`
	// Path to the authorized_keys file. Required when type="sshd".
	AuthorizedKeys string `yaml:"authorized_keys,omitempty"`
	// Command to execute on SSH session start (e.g., ["bash", "-l"]). Default drops into a basic shell.
	Shell []string `yaml:"shell,omitempty"`
	// Additional overrides for the listener.
	Options map[string]string `yaml:"options,omitempty"`
}

// Proxy is not needed as a separate type, but if there's any standalone usage,
// Listener covers it. So we remove Proxy.

// Connection is an outbound SSH connection to a peer or standard SSH server.
type Connection struct {
	// A unique identifier for this connection.
	Name string `yaml:"name"`
	// A list of target addresses to attempt dialing in order (e.g., ["user@192.168.1.50:22", "user@public-ip:22"]).
	Targets []string `yaml:"targets"`
	// How long to wait before attempting to reconnect if the session drops (e.g., "10s").
	Retry string `yaml:"retry"`
	// Authentication credentials for the target server(s).
	Auth AuthCfg `yaml:"auth"`
	// Common SSH options applied to all forwards in this connection.
	Options map[string]string `yaml:"options,omitempty"`
	// A list of forwarding sets. Each set establishes its own purely independent physical SSH connection for maximum throughput.
	Forwards []ForwardSet `yaml:"forwards,omitempty"`
}

// ForwardSet represents a distinct SSH connection for a group of port forwards and proxies.
type ForwardSet struct {
	// A unique identifier for this forwarding set.
	Name string `yaml:"name"`
	// Options overrides or adds to connection-level options (e.g., IPQoS).
	Options map[string]string `yaml:"options,omitempty"`
	// Reverse forwards (-R). Listens on the remote peer, traffic exits your local machine.
	Remote []Forward `yaml:"remote,omitempty"`
	// Local forwards (-L). Listens locally, traffic exits the remote peer.
	Local []Forward `yaml:"local,omitempty"`
}

// Forward is a single unified rule for port forwarding or proxying.
type Forward struct {
	// The type of forward. Can be "forward", "socks", or "http". Defaults to "forward".
	Type string `yaml:"type,omitempty" jsonschema:"enum=forward,enum=socks,enum=http"`
	// The address to bind and listen on.
	Bind string `yaml:"bind"`
	// Where to connect traffic to (or upstream for proxies). Optional for socks/http.
	Target string `yaml:"target,omitempty"`
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
	// Path to the private SSH key.
	Key string `yaml:"key"`
	// Path to the known_hosts file.
	KnownHosts string `yaml:"known_hosts"`
}

// LogCfg configures logging behavior.
type LogCfg struct {
	// Log level: "debug", "info", "warn", or "error". Defaults to "info".
	Level string `yaml:"level,omitempty" jsonschema:"enum=debug,enum=info,enum=warn,enum=error"`
}

// Load reads, parses, and validates a config file.
func Load(path string) (*Config, error) {
	cfg, err := LoadUnvalidated(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadUnvalidated reads and parses a config file without checking for runtime requirements (like file existence).
func LoadUnvalidated(path string) (*Config, error) {
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
	for i := range cfg.Listeners {
		if cfg.Listeners[i].Type == "sshd" {
			cfg.Listeners[i].HostKey = expandHome(cfg.Listeners[i].HostKey)
			cfg.Listeners[i].AuthorizedKeys = expandHome(cfg.Listeners[i].AuthorizedKeys)
		}
	}
	for i := range cfg.Connections {
		cfg.Connections[i].Auth.Key = expandHome(cfg.Connections[i].Auth.Key)
		cfg.Connections[i].Auth.KnownHosts = expandHome(cfg.Connections[i].Auth.KnownHosts)

		// Apply default type="forward" for all unified mappings
		for j := range cfg.Connections[i].Forwards {
			for k := range cfg.Connections[i].Forwards[j].Remote {
				if cfg.Connections[i].Forwards[j].Remote[k].Type == "" {
					cfg.Connections[i].Forwards[j].Remote[k].Type = "forward"
				}
			}
			for k := range cfg.Connections[i].Forwards[j].Local {
				if cfg.Connections[i].Forwards[j].Local[k].Type == "" {
					cfg.Connections[i].Forwards[j].Local[k].Type = "forward"
				}
			}
		}
	}

	if cfg.Name == "" {
		cfg.Name = "mesh"
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Name == "" {
		c.Name = "mesh"
	}

	for i, l := range c.Listeners {
		if l.Bind == "" {
			return fmt.Errorf("listeners[%d] %q: bind is required", i, l.Name)
		}
		if l.Type == "" {
			return fmt.Errorf("listeners[%d] %q: type is required", i, l.Name)
		}
		switch l.Type {
		case "sshd":
			if err := requireFile(l.HostKey, "host_key"); err != nil {
				return fmt.Errorf("listeners[%d] %q: %w", i, l.Name, err)
			}
			if err := requireFile(l.AuthorizedKeys, "authorized_keys"); err != nil {
				return fmt.Errorf("listeners[%d] %q: %w", i, l.Name, err)
			}
		case "relay":
			if l.Target == "" {
				return fmt.Errorf("listeners[%d] %q: target is required for relay", i, l.Name)
			}
		case "socks", "http":
			// Ok
		default:
			return fmt.Errorf("listeners[%d] %q: unknown type %q", i, l.Name, l.Type)
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
			if err := validateForwards(fset.Remote); err != nil {
				return fmt.Errorf("connections[%d] %q forwards[%d] %q remote: %w", i, conn.Name, j, fset.Name, err)
			}
			if err := validateForwards(fset.Local); err != nil {
				return fmt.Errorf("connections[%d] %q forwards[%d] %q local: %w", i, conn.Name, j, fset.Name, err)
			}
		}
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
	for i, l := range c.Listeners {
		if err := check(l.Bind, fmt.Sprintf("listeners[%d]", i)); err != nil {
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

func validateForwards(fwds []Forward) error {
	for i, f := range fwds {
		if f.Bind == "" {
			return fmt.Errorf("[%d]: bind is required", i)
		}
		if f.Type != "forward" && f.Type != "socks" && f.Type != "http" {
			return fmt.Errorf("[%d]: type must be 'forward', 'socks', or 'http'", i)
		}
		if f.Type == "forward" && f.Target == "" {
			return fmt.Errorf("[%d]: target is required for forward type", i)
		}
	}
	// Fallback defaulting is handled during Unmarshal or manually if Type is empty.
	// Since we changed it to explicit, validate requires Type. But wait, we should apply a default if empty.
	// We can do that by mutating it, but `validForwards` takes a slice by value so it doesn't mutate strings inside.
	// It's better to expect `Type` to be populated or return an error, but let's handle defaulting in Unmarshal or here.
	// To safely default `Type="forward"`, we can do it in `LoadUnvalidated` or here if we pass a pointer/slice reference. Let's do it in `LoadUnvalidated`.
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
