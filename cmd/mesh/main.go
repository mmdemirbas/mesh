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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
	"golang.org/x/term"
)

var version = "dev"

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
	ring := newLogRing(10)
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
			c := tunnel.NewSSHClient(conn, nodeName, log)
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
		s, _ := renderStatus(cfg, state.Global.Snapshot(), state.Global.SnapshotMetrics(), nodeName)
		fmt.Print(s)
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
		s, _ := renderStatus(cfg, fetchState(nodeName), nil, nodeName)
		fmt.Print(s)
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
		statusOutput, _ := renderStatus(cfg, fetchState(nodeName), nil, nodeName)
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

	s, _ := renderStatus(cfg, nil, nil, nodeName)
	fmt.Print(s)
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
