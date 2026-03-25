package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/lmittmann/tint"
	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
	"golang.org/x/term"
)

var version = "dev"

var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ANSI color codes. Disabled when NO_COLOR env var is set (https://no-color.org/).
var (
	cReset   = "\033[0m"
	cBold    = "\033[1m"
	cRed     = "\033[31m"
	cGreen   = "\033[32m"
	cYellow  = "\033[33m"
	cBlue    = "\033[34m"
	cMagenta = "\033[35m"
	cCyan    = "\033[36m"
	cGray    = "\033[90m"
	cBlink   = "\033[5m"
)

func init() {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		cReset = ""
		cBold = ""
		cRed = ""
		cGreen = ""
		cYellow = ""
		cBlue = ""
		cMagenta = ""
		cCyan = ""
		cGray = ""
		cBlink = ""
	}
}

// addrKey is a pre-parsed, comparable sort key for an address string.
// The IP is stored as two uint64s for single-instruction comparison on 64-bit CPUs.
type addrKey struct {
	ipHi  uint64 // upper 8 bytes of IPv6/mapped-IPv4
	ipLo  uint64 // lower 8 bytes
	port  uint16
	hasIP bool
	raw   string // original string, used as fallback for non-IP addresses
}

// makeAddrKey parses an address string into a sort key.
// For the common case of [user@]IPv4:port, it does a single-pass parse with
// no calls to net.SplitHostPort, net.ParseIP, or strconv.Atoi.
func makeAddrKey(s string) addrKey {
	raw := s
	// Strip user@ prefix
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			s = s[i+1:]
			break
		}
	}

	// Fast path: try to parse entire "IPv4:port" in one scan
	if k, ok := parseIPv4Port(s, raw); ok {
		return k
	}

	// Slow path: IPv6 or hostname — use stdlib
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		host = s
		portStr = ""
	}
	port := atoiUint16(portStr)
	k := addrKey{port: port, raw: raw}

	if ip := net.ParseIP(host); ip != nil {
		ip16 := ip.To16()
		k.ipHi = uint64(ip16[0])<<56 | uint64(ip16[1])<<48 | uint64(ip16[2])<<40 | uint64(ip16[3])<<32 |
			uint64(ip16[4])<<24 | uint64(ip16[5])<<16 | uint64(ip16[6])<<8 | uint64(ip16[7])
		k.ipLo = uint64(ip16[8])<<56 | uint64(ip16[9])<<48 | uint64(ip16[10])<<40 | uint64(ip16[11])<<32 |
			uint64(ip16[12])<<24 | uint64(ip16[13])<<16 | uint64(ip16[14])<<8 | uint64(ip16[15])
		k.hasIP = true
	}
	return k
}

// parseIPv4Port parses "A.B.C.D:port" in a single scan.
// Returns (key, true) on success. On failure returns (_, false).
func parseIPv4Port(s, raw string) (addrKey, bool) {
	var ip [4]byte
	octet := 0
	dots := 0
	digits := 0
	port := 0
	inPort := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if inPort {
				if port > 6553 || (port == 6553 && c > '5') {
					return addrKey{}, false // prevent overflow before multiplication
				}
				port = port*10 + int(c-'0')
			} else {
				octet = octet*10 + int(c-'0')
				if octet > 255 {
					return addrKey{}, false
				}
				digits++
			}
		} else if c == '.' && !inPort {
			if digits == 0 || dots >= 3 {
				return addrKey{}, false
			}
			ip[dots] = byte(octet)
			dots++
			octet = 0
			digits = 0
		} else if c == ':' && !inPort && dots == 3 && digits > 0 {
			ip[3] = byte(octet)
			inPort = true
		} else {
			return addrKey{}, false
		}
	}

	// Handle bare IPv4 without port (e.g., "10.0.0.1")
	if !inPort {
		if dots != 3 || digits == 0 {
			return addrKey{}, false
		}
		ip[3] = byte(octet)
	}

	// IPv4-mapped IPv6: ::ffff:A.B.C.D stored as uint64 pair
	k := addrKey{
		ipHi:  0,
		ipLo:  uint64(0xff)<<40 | uint64(0xff)<<32 | uint64(ip[0])<<24 | uint64(ip[1])<<16 | uint64(ip[2])<<8 | uint64(ip[3]),
		port:  uint16(port),
		hasIP: true,
		raw:   raw,
	}
	return k, true
}

// parseIPv4 parses an IPv4 dotted-quad without allocation.
// Returns [4]byte{} on failure (caller must check for "0.0.0.0" separately).
func parseIPv4(s string) [4]byte {
	var ip [4]byte
	octet := 0
	dots := 0
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			octet = octet*10 + int(c-'0')
			if octet > 255 {
				return [4]byte{}
			}
			digits++
		} else if c == '.' {
			if digits == 0 || dots >= 3 {
				return [4]byte{}
			}
			ip[dots] = byte(octet)
			dots++
			octet = 0
			digits = 0
		} else {
			return [4]byte{}
		}
	}
	if dots != 3 || digits == 0 {
		return [4]byte{}
	}
	ip[3] = byte(octet)
	return ip
}

// atoiUint16 parses a small non-negative integer without allocation or error handling.
func atoiUint16(s string) uint16 {
	var n uint16
	for i := 0; i < len(s); i++ {
		n = n*10 + uint16(s[i]-'0')
	}
	return n
}

func (k addrKey) less(other addrKey) bool {
	if k.hasIP && other.hasIP {
		if k.ipHi != other.ipHi {
			return k.ipHi < other.ipHi
		}
		if k.ipLo != other.ipLo {
			return k.ipLo < other.ipLo
		}
		return k.port < other.port
	}
	if k.raw != other.raw {
		return k.raw < other.raw
	}
	return k.port < other.port
}

// parseAddr extracts the IP and port from an address string.
// Handles "host:port", "user@host:port", or just "host".
func parseAddr(s string) (net.IP, int) {
	k := makeAddrKey(s)
	if !k.hasIP {
		return nil, int(k.port)
	}
	ip := make(net.IP, 16)
	ip[0] = byte(k.ipHi >> 56)
	ip[1] = byte(k.ipHi >> 48)
	ip[2] = byte(k.ipHi >> 40)
	ip[3] = byte(k.ipHi >> 32)
	ip[4] = byte(k.ipHi >> 24)
	ip[5] = byte(k.ipHi >> 16)
	ip[6] = byte(k.ipHi >> 8)
	ip[7] = byte(k.ipHi)
	ip[8] = byte(k.ipLo >> 56)
	ip[9] = byte(k.ipLo >> 48)
	ip[10] = byte(k.ipLo >> 40)
	ip[11] = byte(k.ipLo >> 32)
	ip[12] = byte(k.ipLo >> 24)
	ip[13] = byte(k.ipLo >> 16)
	ip[14] = byte(k.ipLo >> 8)
	ip[15] = byte(k.ipLo)
	return ip, int(k.port)
}

// compareAddr compares two address strings semantically by IP then port.
func compareAddr(a, b string) bool {
	return makeAddrKey(a).less(makeAddrKey(b))
}

func main() {
	var configPath string
	var watchMode bool
	var showVersion bool
	flag.StringVar(&configPath, "f", "", "Path to config file")
	flag.StringVar(&configPath, "file", "", "Path to config file")
	flag.BoolVar(&watchMode, "w", false, "Watch mode: continuously refresh (for status command)")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Usage = printUsage
	flag.Parse()

	if showVersion {
		fmt.Println("mesh " + version)
		return
	}

	args := flag.Args()

	// Commands that don't require a node name
	if len(args) >= 1 && args[0] == "completion" {
		shell := ""
		if len(args) >= 2 {
			shell = args[1]
		}
		completionCmd(shell)
		return
	}

	if len(args) < 2 {
		printUsage()
		os.Exit(1)
	}

	nodeName := args[0]
	command := args[1]

	if configPath == "" {
		configPath = getDefaultConfigPath()
	}

	switch command {
	case "up":
		upCmd(nodeName, configPath)
	case "status":
		statusCmd(nodeName, configPath, watchMode)
	case "config":
		configCmd(nodeName, configPath)
	case "down":
		downCmd(nodeName)
	case "help":
		printHelp()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(cBold + cCyan + "mesh" + cReset + " " + cGray + version + cReset + " - Human-friendly networking tool")
	fmt.Println()
	fmt.Println("All-in-one replacement for ssh, sshd, autossh, socat, and SOCKS/HTTP proxy servers.")
	fmt.Println()
	fmt.Println(cBold + "Usage:" + cReset)
	fmt.Println("  mesh " + cYellow + "[-f config.yaml] " + cReset + cCyan + "<node> <command>" + cReset + " [arguments]")
	fmt.Println()
	fmt.Println(cBold + "Commands:" + cReset)
	fmt.Println("  " + cBlue + "up" + cReset + "         Start the specified mesh node (live dashboard when running in a terminal)")
	fmt.Println("  " + cBlue + "down" + cReset + "       Stop the currently running mesh node")
	fmt.Println("  " + cBlue + "status" + cReset + "     Show live status of a running node (use " + cYellow + "-w" + cReset + " for watch mode)")
	fmt.Println("  " + cBlue + "config" + cReset + "     Show the parsed configuration for a node without starting it")
	fmt.Println("  " + cBlue + "completion" + cReset + " Generate shell completion script (bash, zsh, fish)")
	fmt.Println()
	fmt.Println(cBold + "Examples:" + cReset)
	fmt.Println("  " + cGray + "# Start the 'server' node using the default configuration file" + cReset)
	fmt.Println("  mesh server " + cBlue + "up" + cReset + " &")
	fmt.Println()
	fmt.Println("  " + cGray + "# Start utilizing a specific configuration file" + cReset)
	fmt.Println("  mesh " + cYellow + "-f" + cReset + " configs/example.yml server " + cBlue + "up" + cReset)
	fmt.Println()
	fmt.Println("  " + cGray + "# Gracefully stop the 'server' node" + cReset)
	fmt.Println("  mesh server " + cBlue + "down" + cReset)
	fmt.Println()
}

func printHelp() {
	printUsage()
	fmt.Println(cBold + "Command Details:" + cReset)
	fmt.Println()
	fmt.Println(cBold + "  up" + cReset)
	fmt.Println("    Starts all configured listeners, connections, and clipsync for the node.")
	fmt.Println("    When running in a terminal, shows a live dashboard that auto-refreshes.")
	fmt.Println("    Logs are written to " + cGray + "~/.mesh/log/<node>.log" + cReset + ".")
	fmt.Println("    When stdout is not a terminal (piped or backgrounded), logs go to stderr.")
	fmt.Println("    Press Ctrl+C to stop gracefully.")
	fmt.Println()
	fmt.Println(cBold + "  status" + cReset + " [" + cYellow + "-w" + cReset + "]")
	fmt.Println("    Shows the current status of a running node (listeners, connections, peers).")
	fmt.Println("    Use " + cYellow + "-w" + cReset + " for watch mode: continuously refreshes like 'top'.")
	fmt.Println("    Without " + cYellow + "-w" + cReset + ", prints once and exits.")
	fmt.Println()
	fmt.Println(cBold + "  config" + cReset)
	fmt.Println("    Displays the parsed configuration for a node without starting it.")
	fmt.Println("    Useful for verifying config changes before running 'up'.")
	fmt.Println("    If the node is not found, lists all available nodes in the config file.")
	fmt.Println()
	fmt.Println(cBold + "  down" + cReset)
	fmt.Println("    Sends SIGTERM to the running node and waits for graceful shutdown.")
	fmt.Println()
	fmt.Println(cBold + "  help" + cReset)
	fmt.Println("    Shows this detailed help.")
	fmt.Println()
	fmt.Println(cBold + "SSH Options:" + cReset)
	fmt.Println("  The following OpenSSH-compatible options can be set in listener/connection/forward config:")
	fmt.Println()
	for _, opt := range []struct{ name, desc string }{
		{"Ciphers", "Encryption algorithms (e.g., aes256-ctr,chacha20-poly1305@openssh.com)"},
		{"MACs", "Message authentication codes (e.g., hmac-sha2-256,hmac-sha2-512)"},
		{"KexAlgorithms", "Key exchange methods (e.g., curve25519-sha256)"},
		{"HostKeyAlgorithms", "Accepted server host key types (e.g., ssh-ed25519,rsa-sha2-256)"},
		{"ConnectTimeout", "Seconds before connection attempt times out"},
		{"IPQoS", "IP QoS/DSCP (e.g., lowdelay, throughput, af11 ef)"},
		{"TCPKeepAlive", "OS-level TCP keepalive interval in seconds"},
		{"ServerAliveInterval", "Client keepalive interval in seconds"},
		{"ServerAliveCountMax", "Max unanswered keepalives before disconnect"},
		{"ClientAliveInterval", "Server keepalive interval in seconds"},
		{"ClientAliveCountMax", "Max unanswered keepalives before disconnect"},
		{"ExitOnForwardFailure", "Stop reconnection if a forward fails (yes/no)"},
		{"GatewayPorts", "Remote forward bind policy (yes/no/clientspecified)"},
		{"PermitOpen", "Restrict tunneled destinations (e.g., *:22, host:80, none)"},
		{"RekeyLimit", "Bytes before SSH re-keying (e.g., 1G, 500M)"},
	} {
		fmt.Printf("    %s%-24s%s %s\n", cCyan, opt.name, cReset, opt.desc)
	}
	fmt.Println()
}

func getDefaultConfigPath() string {
	// 1. Check current directory (prioritize yaml over yml)
	if _, err := os.Stat("mesh.yaml"); err == nil {
		return "mesh.yaml"
	}
	if _, err := os.Stat("mesh.yml"); err == nil {
		return "mesh.yml"
	}

	// 2. Check ~/.mesh/conf
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".mesh", "conf", "mesh.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		p = filepath.Join(home, ".mesh", "conf", "mesh.yml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Fallback to default
	return "mesh.yaml"
}

func upCmd(nodeName, configPath string) {
	pid, err := readPidFile(nodeName)
	if err == nil && pid != 0 && checkPid(pid) {
		fmt.Printf("⨯ mesh node %q is already running (pid %d).\n", nodeName, pid)
		os.Exit(1)
	}

	// Determine whether to use the live dashboard (TTY) or classic log-to-stderr mode.
	useDashboard := term.IsTerminal(int(os.Stdout.Fd()))

	// Phase 1: Bootstrap logger to stderr so config errors are visible.
	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&humanLogHandler{Handler: logHandler})
	slog.SetDefault(log)

	cfg, err := config.Load(configPath, nodeName)
	if err != nil {
		log.Error("Config load failed", "path", configPath, "error", err)
		os.Exit(1)
	}

	var logLevel slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	// Phase 2: Set up log destination — file (dashboard mode) or stderr (classic mode).
	ring := newLogRing(15)
	var logFilePath string

	if useDashboard {
		home, _ := os.UserHomeDir()
		logDir := filepath.Join(home, ".mesh", "log")
		_ = os.MkdirAll(logDir, 0755)
		logFilePath = filepath.Join(logDir, nodeName+".log")
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not open log file %s: %v (falling back to stderr)\n", logFilePath, err)
			useDashboard = false
			logFilePath = ""
		} else {
			defer logFile.Close()
			// File gets plain text, ring gets colored text for the dashboard.
			// humanLogHandler inlines attrs into the message for readability.
			logHandler = tint.NewHandler(logFile, &tint.Options{
				Level:      logLevel,
				TimeFormat: "15:04:05.000",
				NoColor:    true,
			})
			colorHandler := tint.NewHandler(ring, &tint.Options{
				Level:      logLevel,
				TimeFormat: "15:04:05.000",
			})
			log = slog.New(&humanLogHandler{Handler: &multiHandler{plain: logHandler, color: colorHandler}})
			slog.SetDefault(log)
		}
	}

	if !useDashboard {
		logHandler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:      logLevel,
			TimeFormat: "15:04:05.000",
		})
		log = slog.New(&humanLogHandler{Handler: logHandler})
		slog.SetDefault(log)
	}

	log.Info("mesh starting", "version", version, "node", nodeName, "config", configPath)

	if err := writePidFile(nodeName); err != nil {
		log.Error("Failed to write pidfile", "error", err)
	} else {
		defer removePidFile(nodeName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("Shutting down", "signal", sig)
		cancel()
	}()

	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		port := adminLn.Addr().(*net.TCPAddr).Port
		_ = os.WriteFile(portFilePath(nodeName), []byte(strconv.Itoa(port)), 0600)
		defer os.Remove(portFilePath(nodeName))

		adminSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state.Global.Snapshot())
		})}
		go func() { _ = adminSrv.Serve(adminLn) }()
		context.AfterFunc(ctx, func() { adminSrv.Close() })
	}

	var wg sync.WaitGroup

	// 1. Listeners (proxies, relays, ssh servers)
	var proxies, relays []config.Listener
	for _, l := range cfg.Listeners {
		switch l.Type {
		case "socks", "http":
			proxies = append(proxies, l)
		case "relay":
			relays = append(relays, l)
		case "sshd":
			l := l
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := tunnel.NewSSHServer(l, log)
				if err := s.Run(ctx); err != nil {
					log.Error("SSH server failed", "listen", l.Bind, "error", err)
				}
			}()
		}
	}
	if len(proxies) > 0 {
		proxy.RunStandaloneProxies(ctx, proxies, log, &wg)
	}
	if len(relays) > 0 {
		proxy.RunStandaloneRelays(ctx, relays, log, &wg)
	}

	// 2. Outbound connections (Multi-set forwards)
	for _, conn := range cfg.Connections {
		conn := conn
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := tunnel.NewSSHClient(conn, log)
			if err := c.Run(ctx); err != nil {
				log.Error("Connection failed", "name", conn.Name, "error", err)
			}
		}()
	}

	// 3. Clipsync
	for _, cs := range cfg.Clipsync {
		cs := cs
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := clipsync.Start(ctx, cs)
			if err != nil {
				log.Error("Clipsync failed to start", "error", err)
			}
		}()
	}

	// 4. Live dashboard or block until signal
	if useDashboard {
		go runDashboard(ctx, cfg, nodeName, configPath, logFilePath, ring)
	}

	<-ctx.Done()

	wg.Wait()
	log.Info("mesh gracefully stopped")

	if useDashboard {
		// The deferred runDashboard cleanup restores the original screen (alternate buffer exit).
		// Print the final static status to the normal terminal so the user sees the shutdown state.
		// Small delay to let the alternate screen buffer exit complete.
		time.Sleep(50 * time.Millisecond)
		fmt.Print(renderStatus(cfg, state.Global.Snapshot(), nodeName))
	}
}

// humanLogHandler rewrites slog records into natural prose before passing them
// to the underlying handler. Instead of "Connected target=root@10.0.0.1:22 tcp=45ms ssh=120ms"
// it produces "Connected to root@10.0.0.1:22 (tcp: 45ms, ssh: 120ms)".
//
// Strategy: known attribute keys are consumed and inlined into the message.
// Any remaining attributes pass through to the underlying handler unchanged.
type humanLogHandler struct {
	slog.Handler
}

func (h *humanLogHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})

	// Build a human-readable message by consuming known attributes.
	var msg strings.Builder
	msg.WriteString(r.Message)
	consumed := map[string]bool{}

	// Inline "target", "bind", "addr", "peer", "remote", "listen" into the message
	for _, key := range []string{"target", "bind", "addr", "peer", "remote", "listen", "name", "file", "path"} {
		if v, ok := attrs[key]; ok {
			msg.WriteString(" " + v.String())
			consumed[key] = true
		}
	}

	// Group timing/detail attrs as parenthetical
	var details []string
	for _, key := range []string{"tcp", "ssh", "elapsed", "retry_in", "version", "node", "config", "signal",
		"type", "set", "formats", "files", "count", "size", "user", "from", "status", "fail_count"} {
		if v, ok := attrs[key]; ok {
			details = append(details, key+": "+v.String())
			consumed[key] = true
		}
	}
	if len(details) > 0 {
		msg.WriteString(" (" + strings.Join(details, ", ") + ")")
	}

	// "error" goes last, separated
	if v, ok := attrs["error"]; ok {
		msg.WriteString(": " + v.String())
		consumed["error"] = true
	}

	// Rebuild record with new message and only unconsumed attributes
	r.Message = msg.String()
	var remaining []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		if !consumed[a.Key] {
			remaining = append(remaining, a)
		}
		return true
	})

	// Create a clean record without the consumed attrs
	newRecord := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	newRecord.AddAttrs(remaining...)
	return h.Handler.Handle(ctx, newRecord)
}

func (h *humanLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &humanLogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *humanLogHandler) WithGroup(name string) slog.Handler {
	return &humanLogHandler{Handler: h.Handler.WithGroup(name)}
}

// multiHandler fans out log records to two handlers: one for the log file (plain)
// and one for the dashboard ring buffer (colored).
type multiHandler struct {
	plain slog.Handler
	color slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.plain.Enabled(ctx, level) || h.color.Enabled(ctx, level)
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.plain.Enabled(ctx, r.Level) {
		_ = h.plain.Handle(ctx, r)
	}
	if h.color.Enabled(ctx, r.Level) {
		_ = h.color.Handle(ctx, r)
	}
	return nil
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{plain: h.plain.WithAttrs(attrs), color: h.color.WithAttrs(attrs)}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{plain: h.plain.WithGroup(name), color: h.color.WithGroup(name)}
}

// logRing is a fixed-size ring buffer that captures recent log lines for the dashboard.
type logRing struct {
	mu    sync.Mutex
	lines []string
	size  int
	pos   int
	full  bool
}

func newLogRing(size int) *logRing {
	return &logRing{lines: make([]string, size), size: size}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Split into lines; log handlers write one record at a time but may include a trailing newline.
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		r.lines[r.pos] = line
		r.pos = (r.pos + 1) % r.size
		if r.pos == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Lines returns the buffered log lines in chronological order.
func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string{}, r.lines[:r.pos]...)
	}
	out := make([]string, 0, r.size)
	out = append(out, r.lines[r.pos:]...)
	out = append(out, r.lines[:r.pos]...)
	return out
}

// runDashboard renders a live status screen that refreshes periodically.
// It uses the terminal's alternate screen buffer (like vim, top, htop) so the
// dashboard doesn't pollute scrollback and the user's previous terminal content
// is restored when the dashboard exits. Rendering overwrites in-place line by
// line to avoid flicker — no full screen clear is needed.
func runDashboard(ctx context.Context, cfg *config.Config, nodeName string, configPath string, logFilePath string, ring *logRing) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Enter alternate screen buffer and hide cursor
	fmt.Print("\033[?1049h\033[?25l")
	defer fmt.Print("\033[?25h\033[?1049l") // show cursor, leave alternate screen

	render := func() {
		var lines []string

		// Header
		header := fmt.Sprintf("%s%smesh %s%s | pid %d | %s",
			cBold, cCyan, nodeName, cReset, os.Getpid(), time.Now().Format("15:04:05"))
		if configPath != "" {
			header += fmt.Sprintf(" | config: %s%s%s", cGray, configPath, cReset)
		}
		if logFilePath != "" {
			header += fmt.Sprintf(" | log: %s%s%s", cGray, logFilePath, cReset)
		}
		lines = append(lines, header, "")

		// Status body
		statusOutput := renderStatus(cfg, state.Global.Snapshot(), nodeName)
		lines = append(lines, strings.Split(strings.TrimRight(statusOutput, "\n"), "\n")...)

		// Log tail — fill remaining terminal height
		logLines := ring.Lines()
		if len(logLines) > 0 {
			termHeight := 24
			if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 {
				termHeight = h
			}
			available := termHeight - len(lines) - 1 // -1 for separator
			if available > len(logLines) {
				available = len(logLines)
			}
			if available > 0 {
				lines = append(lines, cGray+strings.Repeat("─", 80)+cReset)
				lines = append(lines, logLines[len(logLines)-available:]...)
			}
		}

		// Overwrite in-place: cursor home, then each line + clear-to-EOL.
		// After all lines, clear from cursor to end of screen (removes stale content).
		var buf strings.Builder
		buf.WriteString("\033[H") // cursor home — no clear
		for _, line := range lines {
			buf.WriteString(line)
			buf.WriteString("\033[K\n") // clear to end of line, then newline
		}
		buf.WriteString("\033[J") // clear from cursor to end of screen
		fmt.Print(buf.String())
	}

	winch := winchSignal()

	render()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			render()
		case <-winch:
			render()
		}
	}
}

// renderStatus builds the status dashboard output as a string.
// It can be called from both the live dashboard (in-process state) and
// the statusCmd (state fetched via HTTP from a running node).
func renderStatus(cfg *config.Config, activeState map[string]state.Component, nodeName string) string {
	var w strings.Builder

	writeln := func(s string) { w.WriteString(s); w.WriteByte('\n') }

	getComponentInfo := func(compType, id string) (string, string, state.Component) {
		if activeState == nil {
			return "⚪️", cGray + "[starting]" + cReset, state.Component{}
		}
		comp, ok := activeState[compType+":"+id]
		if !ok {
			return "⚪️", cGray + "[starting]" + cReset, state.Component{}
		}
		color := cGray
		indicator := "⚪️"
		switch comp.Status {
		case state.Listening, state.Connected:
			color = cGreen
			indicator = "🟢"
		case state.Connecting, state.Retrying:
			color = cYellow
			indicator = "🟡"
		case state.Failed:
			color = cRed
			indicator = "🔴"
		}
		msg := string(comp.Status)
		if comp.Message != "" {
			if comp.Status == state.Failed || comp.Status == state.Retrying {
				msg += " (" + comp.Message + ")"
			}
		}
		return indicator, color + "[" + msg + "]" + cReset, comp
	}

	visibleLen := func(str string) int {
		return utf8.RuneCountInString(ansiStripRe.ReplaceAllString(str, ""))
	}

	colorAddr := func(addr string) string {
		if addr == "" {
			return ""
		}
		idx := strings.LastIndex(addr, ":")
		if idx == -1 {
			return cCyan + addr + cReset
		}
		host := addr[:idx]
		port := addr[idx+1:]
		atIdx := strings.Index(host, "@")
		if atIdx != -1 {
			user := host[:atIdx]
			host = host[atIdx+1:]
			return cGray + user + "@" + cReset + cCyan + host + cReset + cGray + ":" + cReset + cMagenta + port + cReset
		}
		return cCyan + host + cReset + cGray + ":" + cReset + cMagenta + port + cReset
	}

	type row struct {
		isHeader  bool
		text      string
		indent    string
		indicator string
		left      string
		arrow     string
		right     string
		status    string
	}
	var rows []row

	addHeader := func(text string) {
		rows = append(rows, row{isHeader: true, text: text})
	}
	addRow := func(indent, ind, left, arrow, right, st string) {
		rows = append(rows, row{indent: indent, indicator: ind, left: left, arrow: arrow, right: right, status: st})
	}

	arrowRight := cCyan + "──▶" + cReset
	arrowLeft := cMagenta + "◀──" + cReset

	writeln(fmt.Sprintf("%s⚙ Configuration: %s%s%s", cBold, cCyan, nodeName, cReset))
	writeln(cGray + strings.Repeat("─", 80) + cReset)

	// Pre-scan: find the widest bind address among protocol-tagged rows for alignment.
	maxProtoAddr := 0
	for _, l := range cfg.Listeners {
		if l.Type == "socks" || l.Type == "http" || l.Type == "sshd" {
			if n := len(l.Bind); n > maxProtoAddr {
				maxProtoAddr = n
			}
		}
	}
	for _, conn := range cfg.Connections {
		for _, fset := range conn.Forwards {
			for _, fwd := range fset.Local {
				if fwd.Type == "socks" || fwd.Type == "http" {
					if n := len(fwd.Bind); n > maxProtoAddr {
						maxProtoAddr = n
					}
				}
			}
			for _, fwd := range fset.Remote {
				if fwd.Type == "socks" || fwd.Type == "http" {
					if n := len(fwd.Bind); n > maxProtoAddr {
						maxProtoAddr = n
					}
				}
			}
		}
	}

	padForProto := func(colored string) string {
		if pad := maxProtoAddr - visibleLen(colored); pad > 0 {
			return colored + strings.Repeat(" ", pad)
		}
		return colored
	}

	dynamicByParent := make(map[string][]state.Component)
	for k, comp := range activeState {
		if strings.HasPrefix(k, "dynamic:") {
			parts := strings.Split(comp.ID, "|")
			if len(parts) == 2 {
				parentBind := parts[1]
				_, port, err := net.SplitHostPort(parentBind)
				if err == nil {
					dynamicByParent[port] = append(dynamicByParent[port], comp)
				}
				dynamicByParent[parentBind] = append(dynamicByParent[parentBind], comp)
			}
		}
	}

	cleanIPv6 := func(peer string) string {
		peer = strings.ReplaceAll(peer, "[", "")
		peer = strings.ReplaceAll(peer, "]", "")
		if idx := strings.Index(peer, "%"); idx != -1 {
			if colonIdx := strings.LastIndex(peer, ":"); colonIdx != -1 {
				peer = peer[:idx] + peer[colonIdx:]
			} else {
				peer = peer[:idx]
			}
		}
		return peer
	}

	sectionTitle := func(name string) string {
		return cBold + cCyan + name + cReset
	}

	// --- Build rows for each section ---

	if len(cfg.Clipsync) > 0 {
		addHeader(sectionTitle("clipsync"))
		for _, cs := range cfg.Clipsync {
			indicator, st, _ := getComponentInfo("clipsync", cs.Bind)
			addRow("", indicator, colorAddr(cs.Bind), "", "", st)

			type peerEntry struct{ addr, label string }
			var peerList []peerEntry
			prefix := "clipsync-peer:" + cs.Bind + "|"
			if activeState != nil {
				for k, comp := range activeState {
					if strings.HasPrefix(k, prefix) {
						peerList = append(peerList, peerEntry{strings.TrimPrefix(k, prefix), comp.Message})
					}
				}
				sort.Slice(peerList, func(i, j int) bool { return compareAddr(peerList[i].addr, peerList[j].addr) })
			} else {
				for _, addr := range cs.StaticPeers {
					peerList = append(peerList, peerEntry{addr, "static"})
				}
			}
			for _, p := range peerList {
				icon := "~"
				if p.label == "static" {
					icon = "·"
				}
				addRow("   ", icon, colorAddr(p.addr), "", cGray+p.label+cReset, "")
			}
		}
		addHeader("")
	}

	if len(cfg.Listeners) > 0 {
		addHeader(sectionTitle("listeners"))
		for _, l := range cfg.Listeners {
			if l.Type == "sshd" {
				indicator, st, _ := getComponentInfo("server", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				addRow("", indicator, left, "", "", st)
			} else if l.Type == "relay" {
				indicator, st, _ := getComponentInfo("relay", l.Bind)
				addRow("", indicator, colorAddr(l.Bind), arrowRight, colorAddr(l.Target), st)
			} else {
				indicator, st, _ := getComponentInfo("proxy", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				arrow, right := "", ""
				if l.Target != "" {
					right = colorAddr(l.Target)
					arrow = arrowRight
				}
				addRow("", indicator, left, arrow, right, st)
			}

			_, searchPort, err := net.SplitHostPort(l.Bind)
			if err != nil {
				searchPort = l.Bind
			}
			dyns := dynamicByParent[l.Bind]
			if len(dyns) == 0 {
				dyns = dynamicByParent[searchPort]
			}
			if len(dyns) > 0 {
				sort.Slice(dyns, func(i, j int) bool {
					a := strings.SplitN(dyns[i].ID, "|", 2)[0]
					b := strings.SplitN(dyns[j].ID, "|", 2)[0]
					return compareAddr(a, b)
				})
				seenID := make(map[string]bool)
				for _, comp := range dyns {
					if seenID[comp.ID] {
						continue
					}
					seenID[comp.ID] = true
					parts := strings.Split(comp.ID, "|")
					right := colorAddr(cleanIPv6(comp.Message))
					if comp.PeerAddr != "" {
						right += " " + cGray + "(" + comp.PeerAddr + ")" + cReset
					}
					addRow("   ", "~", colorAddr(parts[0]), arrowRight, right, "")
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Connections) > 0 {
		for _, c := range cfg.Connections {
			addHeader(sectionTitle(c.Name))

			type targetInfo struct {
				status   state.Status
				peerAddr string
			}
			connectedTargets := make(map[string]targetInfo)
			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				_, _, comp := getComponentInfo("connection", id)
				if comp.Message != "" {
					existing, seen := connectedTargets[comp.Message]
					// Connected takes priority over Connecting
					if !seen || (comp.Status == state.Connected && existing.status != state.Connected) {
						connectedTargets[comp.Message] = targetInfo{status: comp.Status, peerAddr: comp.PeerAddr}
					}
				}
			}
			for _, t := range c.Targets {
				ind := "○"
				suffix := ""
				if info, ok := connectedTargets[t]; ok {
					switch info.status {
					case state.Connected:
						ind = cGreen + "●" + cReset
						if info.peerAddr != "" {
							suffix = " " + cGray + "(" + info.peerAddr + ")" + cReset
						}
					case state.Connecting, state.Retrying:
						ind = cBlink + cYellow + "●" + cReset
					}
				}
				addRow(" ", ind, colorAddr(t)+suffix, "", "", "")
			}

			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				indicator, st, _ := getComponentInfo("connection", id)
				addRow("", indicator, sectionTitle(fset.Name), "", "", st)

				indent := "   "
				for _, fwd := range fset.Local {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					_, _, comp := getComponentInfo("forward", compID)
					lStr := colorAddr(fwd.Bind)
					if comp.BoundAddr != "" && comp.BoundAddr != fwd.Bind {
						lStr = colorAddr(comp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}
					if fwd.Type == "forward" {
						addRow(indent, "", lStr, arrowRight, colorAddr(fwd.Target), "")
					} else {
						lStr = padForProto(lStr) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						rStr := cGray + "🔒 tunnel" + cReset
						if fwd.Target != "" {
							rStr = colorAddr(fwd.Target)
						}
						addRow(indent, "", lStr, arrowRight, rStr, "")
					}
				}
				for _, fwd := range fset.Remote {
					if fwd.Type == "forward" {
						addRow(indent, "", colorAddr(fwd.Target), arrowLeft, colorAddr(fwd.Bind), "")
					} else {
						lStr := cGray + "🔒 tunnel" + cReset
						if fwd.Target != "" {
							lStr = colorAddr(fwd.Target)
						}
						rStr := padForProto(colorAddr(fwd.Bind)) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						addRow(indent, "", lStr, arrowLeft, rStr, "")
					}
				}
			}
		}
		addHeader("")
	}

	var unmappedDynamic []state.Component
	for k, comp := range activeState {
		if strings.HasPrefix(k, "dynamic:") {
			parts := strings.Split(comp.ID, "|")
			if len(parts) != 2 {
				unmappedDynamic = append(unmappedDynamic, comp)
			}
		}
	}
	if len(unmappedDynamic) > 0 {
		sort.Slice(unmappedDynamic, func(i, j int) bool {
			return compareAddr(unmappedDynamic[i].ID, unmappedDynamic[j].ID)
		})
		addHeader(cMagenta + "dynamic ports (unmapped)" + cReset)
		for _, comp := range unmappedDynamic {
			addRow("", "↳", colorAddr(comp.ID), arrowRight, colorAddr(cleanIPv6(comp.Message)), "")
		}
		addHeader("")
	}

	// --- Layout alignment ---

	maxTotalLeft := 0
	for _, r := range rows {
		if !r.isHeader && r.left != "" && r.arrow != "" {
			l := len(r.indent)
			if r.indicator != "" {
				l += 2
			}
			l += visibleLen(r.left)
			if l > maxTotalLeft {
				maxTotalLeft = l
			}
		}
	}

	maxLineLen := 0
	for i, r := range rows {
		if !r.isHeader {
			line := r.indent
			if r.indicator != "" {
				line += r.indicator + " "
			}
			line += r.left
			if r.arrow != "" || r.right != "" {
				currentLen := visibleLen(line)
				padLen := 0
				if maxTotalLeft > currentLen {
					padLen = maxTotalLeft - currentLen
				}
				line += strings.Repeat(" ", padLen+1) + r.arrow + " " + r.right
			}
			rows[i].text = line
			if l := visibleLen(line); l > maxLineLen {
				maxLineLen = l
			}
		}
	}

	statusPadCol := maxLineLen + 1
	if statusPadCol > 60 {
		statusPadCol = 60
	}

	for _, r := range rows {
		if r.isHeader {
			writeln(r.text)
			continue
		}
		line := r.text
		if r.status != "" {
			lineLen := visibleLen(line)
			if lineLen < statusPadCol {
				line += strings.Repeat(" ", statusPadCol-lineLen)
			} else {
				line += " "
			}
			line += r.status
		}
		writeln(strings.TrimRight(line, " "))
	}

	return w.String()
}

func fetchState(nodeName string) map[string]state.Component {
	portData, err := os.ReadFile(portFilePath(nodeName))
	if err != nil {
		return nil
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/", string(portData)))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var s map[string]state.Component
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func statusCmd(nodeName, configPath string, watch bool) {
	pid, err := readPidFile(nodeName)
	if err != nil || pid == 0 {
		fmt.Printf("%s⨯ mesh node %q is not running.%s\n", cRed, nodeName, cReset)
		os.Exit(3)
	}

	if !checkPid(pid) {
		fmt.Printf("%s⨯ mesh node %q is dead but pidfile exists (pid %d).%s\n", cRed, nodeName, pid, cReset)
		os.Exit(1)
	}

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&humanLogHandler{Handler: logHandler})
	slog.SetDefault(log)

	cfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⚠ Could not load configuration to show details: %v%s\n", cYellow, err, cReset)
		os.Exit(0)
	}
	cfg, ok := cfgs[nodeName]
	if !ok {
		fmt.Printf("%s⚠ Node %q not found in config%s\n", cYellow, nodeName, cReset)
		os.Exit(0)
	}

	if !watch {
		// One-shot mode
		fmt.Printf("%s✔ mesh node %q is running (pid %d).%s\n\n", cGreen, nodeName, pid, cReset)
		fmt.Print(renderStatus(cfg, fetchState(nodeName), nodeName))
		os.Exit(0)
	}

	// Watch mode — alternate screen buffer, overwrite in-place
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	winch := winchSignal()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	fmt.Print("\033[?1049h\033[?25l") // alternate screen, hide cursor
	defer fmt.Print("\033[?25h\033[?1049l")

	render := func() {
		if !checkPid(pid) {
			fmt.Print("\033[?25h\033[?1049l") // restore screen
			fmt.Printf("%s⨯ mesh node %q has stopped.%s\n", cRed, nodeName, cReset)
			os.Exit(0)
		}
		header := fmt.Sprintf("%s✔ mesh node %q is running (pid %d)%s | %s",
			cGreen, nodeName, pid, cReset, time.Now().Format("15:04:05"))
		statusOutput := renderStatus(cfg, fetchState(nodeName), nodeName)
		lines := []string{header, ""}
		lines = append(lines, strings.Split(strings.TrimRight(statusOutput, "\n"), "\n")...)

		var buf strings.Builder
		buf.WriteString("\033[H") // cursor home
		for _, line := range lines {
			buf.WriteString(line)
			buf.WriteString("\033[K\n")
		}
		buf.WriteString("\033[J") // clear to end of screen
		fmt.Print(buf.String())
	}

	render()
	for {
		select {
		case <-sigCh:
			return
		case <-ticker.C:
			render()
		case <-winch:
			render()
		}
	}
}

func configCmd(nodeName, configPath string) {
	cfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⨯ Could not load configuration: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}

	cfg, ok := cfgs[nodeName]
	if !ok {
		// If the requested node doesn't exist, list available nodes
		fmt.Printf("%s⨯ Node %q not found in %s%s\n\n", cRed, nodeName, configPath, cReset)
		fmt.Printf("%sAvailable nodes:%s\n", cBold, cReset)
		for name := range cfgs {
			fmt.Printf("  %s%s%s\n", cCyan, name, cReset)
		}
		os.Exit(1)
	}

	fmt.Print(renderStatus(cfg, nil, nodeName))
}

func downCmd(nodeName string) {
	pid, err := readPidFile(nodeName)
	if err != nil || pid == 0 {
		fmt.Printf("mesh node %q is not running.\n", nodeName)
		return
	}

	if !checkPid(pid) {
		fmt.Printf("mesh node %q is not running (stale pidfile).\n", nodeName)
		removePidFile(nodeName)
		return
	}

	fmt.Printf("Stopping mesh node %q (pid %d)...\n", nodeName, pid)
	if err := killPid(pid, syscall.SIGTERM); err != nil {
		fmt.Printf("Error sending SIGTERM: %v\n", err)
		os.Exit(1)
	}
	// Wait for the process to actually exit (up to 10 seconds)
	for i := 0; i < 100; i++ {
		if !checkPid(pid) {
			removePidFile(nodeName)
			fmt.Println("Stopped.")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("Warning: process did not exit within 10 seconds.")
}

func portFilePath(nodeName string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", fmt.Sprintf("mesh-%s.port", nodeName))
}

func pidFilePath(nodeName string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", fmt.Sprintf("mesh-%s.pid", nodeName))
}

func writePidFile(nodeName string) error {
	pid := os.Getpid()
	data := []byte(strconv.Itoa(pid))
	return os.WriteFile(pidFilePath(nodeName), data, 0600)
}

func readPidFile(nodeName string) (int, error) {
	data, err := os.ReadFile(pidFilePath(nodeName))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func removePidFile(nodeName string) {
	os.Remove(pidFilePath(nodeName))
}

func checkPid(pid int) bool {
	if runtime.GOOS == "windows" {
		// FindProcess always succeeds on Windows. Instead, explicitly poll tasklist.
		cmd := exec.Command("tasklist", "/NH", "/FI", fmt.Sprintf("PID eq %d", pid))
		output, err := cmd.Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(output), strconv.Itoa(pid))
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, sending signal 0 checks if the process exists
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func killPid(pid int, sig syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(sig); err != nil {
		// Fallback to Kill if the OS (e.g. Windows) doesn't support the specific signal.
		return process.Kill()
	}
	return nil
}

func completionCmd(shell string) {
	switch shell {
	case "bash":
		fmt.Print(completionBash)
	case "zsh":
		fmt.Print(completionZsh)
	case "fish":
		fmt.Print(completionFish)
	default:
		fmt.Println(cBold + "Usage:" + cReset + " mesh completion <bash|zsh|fish>")
		fmt.Println()
		fmt.Println("Generate a shell completion script. To load completions:")
		fmt.Println()
		fmt.Println(cBold + "  Bash:" + cReset)
		fmt.Println("    source <(mesh completion bash)")
		fmt.Println("    # Or persist: mesh completion bash > /etc/bash_completion.d/mesh")
		fmt.Println()
		fmt.Println(cBold + "  Zsh:" + cReset)
		fmt.Println("    source <(mesh completion zsh)")
		fmt.Println("    # Or persist: mesh completion zsh > \"${fpath[1]}/_mesh\"")
		fmt.Println()
		fmt.Println(cBold + "  Fish:" + cReset)
		fmt.Println("    mesh completion fish | source")
		fmt.Println("    # Or persist: mesh completion fish > ~/.config/fish/completions/mesh.fish")
		if shell != "" {
			fmt.Fprintf(os.Stderr, "\nUnknown shell: %s\n", shell)
			os.Exit(1)
		}
	}
}

// _mesh_nodes is a helper function name used in completion scripts.
// It parses the config file to extract node names dynamically.
const completionBash = `# bash completion for mesh
_mesh_completions() {
    local cur prev words cword
    _init_completion || return

    local commands="up down status config help"
    local flags="-f --file -w --version"

    # Get node names from config
    _mesh_nodes() {
        local config_file=""
        for ((i=1; i < ${#words[@]}; i++)); do
            if [[ "${words[i]}" == "-f" || "${words[i]}" == "--file" ]] && (( i+1 < ${#words[@]} )); then
                config_file="${words[i+1]}"
                break
            fi
        done

        if [[ -z "$config_file" ]]; then
            for f in mesh.yaml mesh.yml ~/.mesh/conf/mesh.yaml ~/.mesh/conf/mesh.yml; do
                if [[ -f "$f" ]]; then
                    config_file="$f"
                    break
                fi
            done
        fi

        if [[ -n "$config_file" && -f "$config_file" ]]; then
            # Extract top-level YAML keys (node names)
            grep -E '^[a-zA-Z_][a-zA-Z0-9_-]*:' "$config_file" 2>/dev/null | sed 's/:.*//'
        fi
    }

    # Find the node name and command positions (skipping flags and their args)
    local node_pos="" cmd_pos=""
    local skip_next=false
    for ((i=1; i < cword; i++)); do
        if $skip_next; then
            skip_next=false
            continue
        fi
        case "${words[i]}" in
            -f|--file)
                skip_next=true
                continue
                ;;
            -*)
                continue
                ;;
            *)
                if [[ -z "$node_pos" ]]; then
                    node_pos=$i
                elif [[ -z "$cmd_pos" ]]; then
                    cmd_pos=$i
                fi
                ;;
        esac
    done

    # Complete flags anywhere
    if [[ "$cur" == -* ]]; then
        COMPREPLY=($(compgen -W "$flags" -- "$cur"))
        return
    fi

    # After -f/--file, complete file paths
    if [[ "$prev" == "-f" || "$prev" == "--file" ]]; then
        _filedir yaml
        _filedir yml
        return
    fi

    # First positional arg: node name or "completion"
    if [[ -z "$node_pos" ]]; then
        local nodes
        nodes=$(_mesh_nodes)
        COMPREPLY=($(compgen -W "$nodes completion" -- "$cur"))
        return
    fi

    # Second positional arg: command
    if [[ -z "$cmd_pos" ]]; then
        COMPREPLY=($(compgen -W "$commands" -- "$cur"))
        return
    fi
}

complete -F _mesh_completions mesh
`

const completionZsh = `#compdef mesh

_mesh() {
    local -a commands=(
        'up:Start the specified mesh node'
        'down:Stop the currently running mesh node'
        'status:Show live status of a running node'
        'config:Show the parsed configuration for a node'
        'help:Show detailed help'
    )

    _mesh_nodes() {
        local config_file=""
        local -i i
        for ((i=1; i < ${#words[@]}; i++)); do
            if [[ "${words[i]}" == "-f" || "${words[i]}" == "--file" ]] && (( i+1 < ${#words[@]} )); then
                config_file="${words[i+1]}"
                break
            fi
        done

        if [[ -z "$config_file" ]]; then
            for f in mesh.yaml mesh.yml ~/.mesh/conf/mesh.yaml ~/.mesh/conf/mesh.yml; do
                if [[ -f "$f" ]]; then
                    config_file="$f"
                    break
                fi
            done
        fi

        if [[ -n "$config_file" && -f "$config_file" ]]; then
            local -a nodes
            nodes=(${(f)"$(grep -E '^[a-zA-Z_][a-zA-Z0-9_-]*:' "$config_file" 2>/dev/null | sed 's/:.*//')"})
            compadd -a nodes
        fi
    }

    # Find positions of node and command in the current line
    local node_pos="" cmd_pos=""
    local skip_next=false
    local -i i
    for ((i=2; i < CURRENT; i++)); do
        if $skip_next; then
            skip_next=false
            continue
        fi
        case "${words[i]}" in
            -f|--file)
                skip_next=true
                continue
                ;;
            -*)
                continue
                ;;
            *)
                if [[ -z "$node_pos" ]]; then
                    node_pos=$i
                elif [[ -z "$cmd_pos" ]]; then
                    cmd_pos=$i
                fi
                ;;
        esac
    done

    # Complete flags
    if [[ "$words[CURRENT]" == -* ]]; then
        _arguments \
            '(-f --file)'{-f,--file}'[Path to config file]:config file:_files -g "*.y(a|)ml"' \
            '-w[Watch mode for status command]' \
            '--version[Print version and exit]'
        return
    fi

    # After -f/--file, complete file paths
    if [[ "$words[CURRENT-1]" == "-f" || "$words[CURRENT-1]" == "--file" ]]; then
        _files -g '*.y(a|)ml'
        return
    fi

    # First positional: node name or "completion"
    if [[ -z "$node_pos" ]]; then
        _alternative \
            'nodes:node:_mesh_nodes' \
            'completion:completion:(completion)'
        return
    fi

    # If first positional is "completion", complete shell names
    if [[ "${words[node_pos]}" == "completion" ]]; then
        compadd bash zsh fish
        return
    fi

    # Second positional: command
    if [[ -z "$cmd_pos" ]]; then
        _describe 'command' commands
        return
    fi
}

_mesh "$@"
`

const completionFish = `# fish completion for mesh

# Determine config file from command line args or default locations
function __mesh_config_file
    set -l args (commandline -opc)
    for i in (seq 2 (count $args))
        if test "$args[$i]" = "-f" -o "$args[$i]" = "--file"
            set -l next (math $i + 1)
            if test $next -le (count $args)
                echo $args[$next]
                return
            end
        end
    end
    for f in mesh.yaml mesh.yml ~/.mesh/conf/mesh.yaml ~/.mesh/conf/mesh.yml
        if test -f $f
            echo $f
            return
        end
    end
end

# Extract node names from config
function __mesh_nodes
    set -l config (__mesh_config_file)
    if test -n "$config" -a -f "$config"
        grep -E '^[a-zA-Z_][a-zA-Z0-9_-]*:' $config 2>/dev/null | sed 's/:.*//'
    end
end

# Check if a node name has been provided (skip flags and their args)
function __mesh_needs_node
    set -l args (commandline -opc)
    set -l skip_next false
    for i in (seq 2 (count $args))
        if $skip_next
            set skip_next false
            continue
        end
        switch $args[$i]
            case -f --file
                set skip_next true
            case '-*'
                continue
            case '*'
                return 1  # node already provided
        end
    end
    return 0
end

# Check if we need a command (node provided but no command yet)
function __mesh_needs_command
    set -l args (commandline -opc)
    set -l skip_next false
    set -l positionals 0
    for i in (seq 2 (count $args))
        if $skip_next
            set skip_next false
            continue
        end
        switch $args[$i]
            case -f --file
                set skip_next true
            case '-*'
                continue
            case '*'
                set positionals (math $positionals + 1)
        end
    end
    test $positionals -eq 1
end

# Check if first positional is "completion"
function __mesh_is_completion
    set -l args (commandline -opc)
    set -l skip_next false
    for i in (seq 2 (count $args))
        if $skip_next
            set skip_next false
            continue
        end
        switch $args[$i]
            case -f --file
                set skip_next true
            case '-*'
                continue
            case '*'
                test "$args[$i]" = "completion"
                return $status
        end
    end
    return 1
end

# Global flags
complete -c mesh -s f -l file -rF -d 'Path to config file'
complete -c mesh -s w -d 'Watch mode for status command'
complete -c mesh -l version -d 'Print version and exit'

# Node names (first positional)
complete -c mesh -n __mesh_needs_node -f -a '(__mesh_nodes)' -d 'Node name'
complete -c mesh -n __mesh_needs_node -f -a completion -d 'Generate shell completion script'

# Commands (second positional)
complete -c mesh -n __mesh_needs_command -f -a up -d 'Start the specified mesh node'
complete -c mesh -n __mesh_needs_command -f -a down -d 'Stop the currently running mesh node'
complete -c mesh -n __mesh_needs_command -f -a status -d 'Show live status of a running node'
complete -c mesh -n __mesh_needs_command -f -a config -d 'Show the parsed configuration'
complete -c mesh -n __mesh_needs_command -f -a help -d 'Show detailed help'

# Shell names for "completion" subcommand
complete -c mesh -n __mesh_is_completion -f -a 'bash zsh fish' -d 'Shell type'
`
