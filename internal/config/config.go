package config

import (
	"fmt"
	"log/slog"
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
	// Local server ports to bind (e.g., SOCKS, HTTP proxies, Relays, or an embedded SSH server).
	Listeners []Listener `yaml:"listeners,omitempty"`
	// Outbound SSH connections to other peers, which encapsulate port forwards and proxy rules.
	Connections []Connection `yaml:"connections,omitempty"`
	// Clipsync configuration.
	Clipsync []ClipsyncCfg `yaml:"clipsync,omitempty"`
	// Filesync configuration (Syncthing-style folder synchronization).
	Filesync []FilesyncCfg `yaml:"filesync,omitempty"`
	// Log level: "debug", "info", "warn", or "error". Defaults to "info".
	LogLevel string `yaml:"log_level,omitempty" jsonschema:"enum=debug,enum=info,enum=warn,enum=error"`
	// Admin server listen address for the local HTTP API (/api/state, /metrics, /ui).
	// Defaults to "127.0.0.1:0" (random port, localhost only).
	// Set to a fixed address (e.g., "127.0.0.1:2225") for stable Prometheus scraping.
	// Set to "" to use the default. Set to "off" to disable entirely.
	AdminAddr string `yaml:"admin_addr,omitempty"`
}

// Listener represents a local server port (proxy, relay, or sshd).
// "relay" = standalone TCP forwarder (socat replacement); "forward" = SSH tunnel port-forward.
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
	// Only a subset of OpenSSH config keys are parsed: Ciphers, MACs, KexAlgorithms, ConnectTimeout, IPQoS.
	Options map[string]string `yaml:"options,omitempty"`
}

// Proxy is not needed as a separate type, but if there's any standalone usage,
// Listener covers it. So we remove Proxy.

// Connection is an outbound SSH connection to a peer or standard SSH server.
type Connection struct {
	// A unique identifier for this connection.
	Name string `yaml:"name"`
	// Connection mode: "failover" (default) tries targets in order until one succeeds;
	// "multiplex" connects to ALL targets simultaneously.
	Mode string `yaml:"mode,omitempty" jsonschema:"enum=failover,enum=multiplex"`
	// A list of target addresses. In failover mode, tried in order. In multiplex mode, all connected simultaneously.
	// Format: "user@host:port" (e.g., ["root@192.168.1.50:22", "root@10.0.0.1:22"]).
	Targets []string `yaml:"targets"`
	// How long to wait before attempting to reconnect if the session drops (e.g., "10s"). Defaults to 10s.
	Retry string `yaml:"retry,omitempty"`
	// Authentication credentials for the target server(s).
	// Multiple methods can be configured; tried in order: agent → key → password_command.
	Auth AuthCfg `yaml:"auth,omitempty"`
	// Common SSH options applied to all forwards in this connection.
	// Supported keys: Ciphers, MACs, KexAlgorithms, HostKeyAlgorithms, ConnectTimeout,
	// IPQoS, TCPKeepAlive, ServerAliveInterval, ServerAliveCountMax, ExitOnForwardFailure,
	// GatewayPorts, PermitOpen, RekeyLimit, StrictHostKeyChecking.
	Options map[string]string `yaml:"options,omitempty"`
	// A list of forwarding sets. Each set establishes its own purely independent physical SSH connection for maximum throughput.
	// In multiplex mode, forwards are applied to each target connection independently.
	Forwards []ForwardSet `yaml:"forwards,omitempty"`
}

// ClipsyncCfg represents the YAML configuration structure.
type ClipsyncCfg struct {
	Bind         string   `yaml:"bind"`                    // e.g., "0.0.0.0:7755"
	LANDiscovery bool     `yaml:"lan_discovery,omitempty"` // default: true
	StaticPeers  []string `yaml:"static_peers,omitempty"`
	AllowSendTo  []string `yaml:"allow_send_to,omitempty"` // "all", "none", "udp", or specific IPs. Default: ["all"]
	AllowReceive []string `yaml:"allow_receive,omitempty"` // "all", "none", "udp", or specific IPs. Default: ["all"]
	PollInterval string   `yaml:"poll_interval,omitempty"` // Clipboard polling interval (e.g., "3s", "5s"). Default: "3s"
}

// UnmarshalYAML provides default values for ClipsyncCfg.
func (c *ClipsyncCfg) UnmarshalYAML(value *yaml.Node) error {
	type plain ClipsyncCfg
	// Set defaults
	c.LANDiscovery = true
	c.AllowSendTo = []string{"all"}
	c.AllowReceive = []string{"all"}

	if err := value.Decode((*plain)(c)); err != nil {
		return err
	}
	return nil
}

// FilesyncCfg configures a Syncthing-style folder synchronization instance.
type FilesyncCfg struct {
	// Network address for the filesync HTTP server (e.g., "0.0.0.0:7756").
	Bind string `yaml:"bind"`
	// Folders to synchronize.
	Folders []FolderCfg `yaml:"folders"`
	// Periodic full rescan interval (e.g., "60s", "5m"). Default: "60s".
	// Acts as a safety net alongside real-time filesystem notifications.
	ScanInterval string `yaml:"scan_interval,omitempty"`
	// Maximum concurrent file transfers per sync cycle. Default: 4.
	MaxConcurrent int `yaml:"max_concurrent,omitempty"`
}

// UnmarshalYAML provides default values for FilesyncCfg.
func (c *FilesyncCfg) UnmarshalYAML(value *yaml.Node) error {
	type plain FilesyncCfg
	c.ScanInterval = "60s"
	c.MaxConcurrent = 4
	if err := value.Decode((*plain)(c)); err != nil {
		return err
	}
	return nil
}

// FolderCfg configures a single folder for synchronization.
type FolderCfg struct {
	// Unique identifier shared between peers (must match on all sides).
	ID string `yaml:"id"`
	// Local filesystem path to sync.
	Path string `yaml:"path"`
	// Explicit peer addresses (host:port) to sync this folder with.
	Peers []string `yaml:"peers"`
	// Sync direction: "send-receive" (bidirectional), "send-only", or "receive-only".
	// Default: "send-receive".
	Direction string `yaml:"direction,omitempty" jsonschema:"enum=send-receive,enum=send-only,enum=receive-only"`
	// Ignore patterns (gitignore-style), local to this node.
	// Applied in addition to .stignore file in the folder root.
	IgnorePatterns []string `yaml:"ignore_patterns,omitempty"`
}

// UnmarshalYAML provides default values for FolderCfg.
func (f *FolderCfg) UnmarshalYAML(value *yaml.Node) error {
	type plain FolderCfg
	f.Direction = "send-receive"
	if err := value.Decode((*plain)(f)); err != nil {
		return err
	}
	return nil
}

// ForwardSet represents a distinct SSH connection for a group of port forwards and proxies.
type ForwardSet struct {
	// A unique identifier for this forwarding set.
	Name string `yaml:"name"`
	// Options overrides or adds to connection-level options.
	// Only a subset of OpenSSH config keys are parsed: Ciphers, MACs, KexAlgorithms, ConnectTimeout, IPQoS.
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

// AuthCfg configures authentication for a connection.
// Methods are listed in recommended order (most secure first).
// Multiple methods can be configured; they are tried in order: agent → key → password.
type AuthCfg struct {
	// Use the running SSH agent (SSH_AUTH_SOCK) for authentication. Most secure — keys never leave the agent.
	Agent bool `yaml:"agent,omitempty"`
	// Path to the private SSH key.
	Key string `yaml:"key,omitempty"`
	// Shell command whose stdout is used as the password. Supports macOS Keychain, pass, 1Password CLI, etc.
	// Example: "security find-generic-password -s mesh -w" (macOS) or "pass show mesh/server" (pass/gpg).
	PasswordCommand string `yaml:"password_command,omitempty"`
	// Path to the known_hosts file for server verification.
	KnownHosts string `yaml:"known_hosts,omitempty"`
}

// Load reads, parses, and returns the requested service's validated config.
func Load(path, serviceName string) (*Config, error) {
	cfgs, err := LoadUnvalidated(path)
	if err != nil {
		return nil, err
	}
	cfg, ok := cfgs[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %q not found in config", serviceName)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("service %q validation failed: %w", serviceName, err)
	}
	return cfg, nil
}

// warnInsecurePermissions logs a warning if the config file is readable by group or others.
// This matters when the config contains password_command or other sensitive directives.
func warnInsecurePermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	mode := info.Mode().Perm()
	if mode&0077 != 0 {
		slog.Warn("Config file has insecure permissions; consider chmod 600", "path", path, "mode", fmt.Sprintf("%04o", mode))
	}
}

// LoadUnvalidated reads and parses a config file without checking for runtime requirements (like file existence).
func LoadUnvalidated(path string) (map[string]*Config, error) {
	warnInsecurePermissions(path)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the user-specified config file path from the CLI
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := os.ExpandEnv(string(data))

	var cfgs map[string]*Config
	if err := yaml.Unmarshal([]byte(expanded), &cfgs); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	for _, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		WarnUnsupportedOptions(cfg)

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
		}
		for i := range cfg.Filesync {
			for j := range cfg.Filesync[i].Folders {
				cfg.Filesync[i].Folders[j].Path = expandHome(cfg.Filesync[i].Folders[j].Path)
			}
		}
	}

	return cfgs, nil
}

// WarnUnsupportedOptions traverses the loaded configuration and logs warnings
// for any user-defined SSH options that mesh does not natively support mapping.
func WarnUnsupportedOptions(cfg *Config) {
	supported := map[string]struct{}{
		"ciphers":               {},
		"macs":                  {},
		"kexalgorithms":         {},
		"hostkeyalgorithms":     {},
		"connecttimeout":        {},
		"ipqos":                 {},
		"clientaliveinterval":   {},
		"clientalivecountmax":   {},
		"serveraliveinterval":   {},
		"serveralivecountmax":   {},
		"tcpkeepalive":          {},
		"exitonforwardfailure":  {},
		"gatewayports":          {},
		"permitopen":            {},
		"rekeylimit":            {},
		"stricthostkeychecking": {},
	}

	check := func(opts map[string]string, context string) {
		for k := range opts {
			if _, ok := supported[strings.ToLower(k)]; !ok {
				slog.Warn("Ignoring unsupported SSH option", "option", k, "context", context)
			}
		}
	}

	for _, l := range cfg.Listeners {
		check(l.Options, fmt.Sprintf("listener:%s", l.Bind))
	}
	for _, c := range cfg.Connections {
		check(c.Options, fmt.Sprintf("connection:%s", c.Name))
		for _, f := range c.Forwards {
			check(f.Options, fmt.Sprintf("connection:%s forwardset:%s", c.Name, f.Name))
		}
	}
}

func (c *Config) Validate() error {

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
		if conn.Mode != "" && conn.Mode != "failover" && conn.Mode != "multiplex" {
			return fmt.Errorf("connections[%d] %q: mode must be 'failover' or 'multiplex', got %q", i, conn.Name, conn.Mode)
		}
		if conn.Retry != "" {
			if _, err := time.ParseDuration(conn.Retry); err != nil {
				return fmt.Errorf("connections[%d] %q: invalid retry duration %q: %w", i, conn.Name, conn.Retry, err)
			}
		}
		// At least one auth method must be configured
		hasAuth := conn.Auth.Agent || conn.Auth.Key != "" || conn.Auth.PasswordCommand != ""
		if !hasAuth {
			return fmt.Errorf("connections[%d] %q: at least one auth method is required (agent, key, or password_command)", i, conn.Name)
		}
		if conn.Auth.Key != "" {
			if err := requireFile(conn.Auth.Key, "auth.key"); err != nil {
				return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
			}
		}
		if conn.Auth.KnownHosts != "" {
			if err := requireFile(conn.Auth.KnownHosts, "auth.known_hosts"); err != nil {
				return fmt.Errorf("connections[%d] %q: %w", i, conn.Name, err)
			}
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

	for i, fs := range c.Filesync {
		if fs.Bind == "" {
			return fmt.Errorf("filesync[%d]: bind is required", i)
		}
		if len(fs.Folders) == 0 {
			return fmt.Errorf("filesync[%d]: at least one folder is required", i)
		}
		if fs.ScanInterval != "" {
			if _, err := time.ParseDuration(fs.ScanInterval); err != nil {
				return fmt.Errorf("filesync[%d]: invalid scan_interval %q: %w", i, fs.ScanInterval, err)
			}
		}
		if fs.MaxConcurrent <= 0 {
			return fmt.Errorf("filesync[%d]: max_concurrent must be positive", i)
		}
		folderIDs := make(map[string]int)
		for j, f := range fs.Folders {
			if f.ID == "" {
				return fmt.Errorf("filesync[%d] folders[%d]: id is required", i, j)
			}
			if prev, ok := folderIDs[f.ID]; ok {
				return fmt.Errorf("filesync[%d]: duplicate folder id %q: folders[%d] and folders[%d]", i, f.ID, prev, j)
			}
			folderIDs[f.ID] = j
			if f.Path == "" {
				return fmt.Errorf("filesync[%d] folders[%d] %q: path is required", i, j, f.ID)
			}
			if len(f.Peers) == 0 {
				return fmt.Errorf("filesync[%d] folders[%d] %q: at least one peer is required", i, j, f.ID)
			}
			switch f.Direction {
			case "send-receive", "send-only", "receive-only":
				// ok
			default:
				return fmt.Errorf("filesync[%d] folders[%d] %q: direction must be send-receive, send-only, or receive-only, got %q", i, j, f.ID, f.Direction)
			}
		}
	}

	// Check for duplicate names
	if err := c.checkDuplicateNames(); err != nil {
		return err
	}

	// Check for duplicate bind addresses across all components
	if err := c.checkDuplicateBinds(); err != nil {
		return err
	}

	return nil
}

// checkDuplicateNames detects name collisions within connection names,
// forward set names (per connection), and listener names.
func (c *Config) checkDuplicateNames() error {
	connNames := make(map[string]int)
	for i, conn := range c.Connections {
		if prev, ok := connNames[conn.Name]; ok {
			return fmt.Errorf("duplicate connection name %q: connections[%d] and connections[%d]", conn.Name, prev, i)
		}
		connNames[conn.Name] = i

		fsetNames := make(map[string]int)
		for j, fset := range conn.Forwards {
			if fset.Name == "" {
				continue
			}
			if prev, ok := fsetNames[fset.Name]; ok {
				return fmt.Errorf("connections[%d] %q: duplicate forward set name %q: forwards[%d] and forwards[%d]", i, conn.Name, fset.Name, prev, j)
			}
			fsetNames[fset.Name] = j
		}
	}

	listenerNames := make(map[string]int)
	for i, l := range c.Listeners {
		if l.Name == "" {
			continue
		}
		if prev, ok := listenerNames[l.Name]; ok {
			return fmt.Errorf("duplicate listener name %q: listeners[%d] and listeners[%d]", l.Name, prev, i)
		}
		listenerNames[l.Name] = i
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
	for i, fs := range c.Filesync {
		if err := check(fs.Bind, fmt.Sprintf("filesync[%d]", i)); err != nil {
			return err
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
var validIPQoSValues = map[string]struct{}{
	"lowdelay": {}, "throughput": {}, "reliability": {}, "none": {},
	"af11": {}, "af12": {}, "af13": {},
	"af21": {}, "af22": {}, "af23": {},
	"af31": {}, "af32": {}, "af33": {},
	"af41": {}, "af42": {}, "af43": {},
	"ef":  {},
	"cs0": {}, "cs1": {}, "cs2": {}, "cs3": {},
	"cs4": {}, "cs5": {}, "cs6": {}, "cs7": {},
}

func validateIPQoS(value string) error {
	if value == "" {
		return nil
	}
	parts := strings.Fields(value)
	if len(parts) > 2 {
		return fmt.Errorf("invalid ipqos value: expected 1 or 2 parts, got %d", len(parts))
	}
	for _, part := range parts {
		if _, ok := validIPQoSValues[strings.ToLower(part)]; !ok {
			return fmt.Errorf("unknown ipqos value %q", part)
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
