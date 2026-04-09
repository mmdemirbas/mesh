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
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/filesync"
	"github.com/mmdemirbas/mesh/internal/gateway"
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
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	command := args[0]
	nodeArgs := args[1:]

	if configPath == "" {
		configPath = getDefaultConfigPath()
	}

	switch command {
	case "up":
		upCmd(resolveNodes(nodeArgs, configPath), configPath)
	case "status":
		statusCmd(resolveNodes(nodeArgs, configPath), configPath, watchMode)
	case "config":
		configCmd(resolveNodes(nodeArgs, configPath), configPath)
	case "down":
		downCmd(resolveNodes(nodeArgs, configPath))
	case "completion":
		shell := ""
		if len(nodeArgs) >= 1 {
			shell = nodeArgs[0]
		}
		completionCmd(shell)
	case "help":
		printHelp()
	default:
		printUsage()
		os.Exit(1)
	}
}

// resolveNodes returns the node names to operate on.
// If explicit names are given, they are returned as-is.
// Otherwise, all nodes from the config file are returned in sorted order.
func resolveNodes(args []string, configPath string) []string {
	if len(args) > 0 {
		return args
	}
	cfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⨯ Could not load configuration: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	if len(cfgs) == 0 {
		fmt.Printf("%s⨯ No nodes defined in %s%s\n", cRed, configPath, cReset)
		os.Exit(1)
	}
	names := make([]string, 0, len(cfgs))
	for name := range cfgs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func printUsage() {
	fmt.Println(cBold + cCyan + "mesh" + cReset + " " + cGray + version + cReset + " - Human-friendly networking tool")
	fmt.Println()
	fmt.Println("All-in-one replacement for ssh, sshd, autossh, socat, and SOCKS/HTTP proxy servers.")
	fmt.Println()
	fmt.Println(cBold + "Usage:" + cReset)
	fmt.Println("  mesh " + cYellow + "[-f config.yaml] " + cReset + cCyan + "<command>" + cReset + " [node...] [flags]")
	fmt.Println()
	fmt.Println(cBold + "Commands:" + cReset)
	fmt.Println("  " + cBlue + "up" + cReset + "         Start mesh nodes (live dashboard when running in a terminal)")
	fmt.Println("  " + cBlue + "down" + cReset + "       Stop running mesh nodes")
	fmt.Println("  " + cBlue + "status" + cReset + "     Show live status of running nodes (use " + cYellow + "-w" + cReset + " for watch mode)")
	fmt.Println("  " + cBlue + "config" + cReset + "     Show the parsed configuration for nodes without starting them")
	fmt.Println("  " + cBlue + "completion" + cReset + " Generate shell completion script (bash, zsh, fish)")
	fmt.Println()
	fmt.Println("  When no node names are given, all nodes in the config file are used.")
	fmt.Println()
	fmt.Println(cBold + "Examples:" + cReset)
	fmt.Println("  " + cGray + "# Start the 'server' node" + cReset)
	fmt.Println("  mesh " + cBlue + "up" + cReset + " server &")
	fmt.Println()
	fmt.Println("  " + cGray + "# Start all nodes from a specific configuration file" + cReset)
	fmt.Println("  mesh " + cYellow + "-f" + cReset + " configs/example.yml " + cBlue + "up" + cReset)
	fmt.Println()
	fmt.Println("  " + cGray + "# Gracefully stop the 'server' node" + cReset)
	fmt.Println("  mesh " + cBlue + "down" + cReset + " server")
	fmt.Println()
}

func printHelp() {
	printUsage()
	fmt.Println(cBold + "Command Details:" + cReset)
	fmt.Println()
	fmt.Println(cBold + "  up" + cReset + " [node...]")
	fmt.Println("    Starts all configured listeners, connections, and clipsync for the given nodes.")
	fmt.Println("    Without node names, starts all nodes defined in the config file.")
	fmt.Println("    Multiple nodes run within a single process.")
	fmt.Println("    When running in a terminal, shows a live dashboard that auto-refreshes.")
	fmt.Println("    Logs are written to " + cGray + "~/.mesh/log/<node>.log" + cReset + ".")
	fmt.Println("    When stdout is not a terminal (piped or backgrounded), logs go to stderr.")
	fmt.Println("    Press Ctrl+C to stop gracefully.")
	fmt.Println()
	fmt.Println(cBold + "  status" + cReset + " [node...] [" + cYellow + "-w" + cReset + "]")
	fmt.Println("    Shows the current status of running nodes (listeners, connections, peers).")
	fmt.Println("    Use " + cYellow + "-w" + cReset + " for watch mode: continuously refreshes like 'top'.")
	fmt.Println("    Without " + cYellow + "-w" + cReset + ", prints once and exits.")
	fmt.Println()
	fmt.Println(cBold + "  config" + cReset + " [node...]")
	fmt.Println("    Displays the parsed configuration for the given nodes without starting them.")
	fmt.Println("    Useful for verifying config changes before running 'up'.")
	fmt.Println()
	fmt.Println(cBold + "  down" + cReset + " [node...]")
	fmt.Println("    Sends SIGTERM to the running nodes and waits for graceful shutdown.")
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

func upCmd(nodeNames []string, configPath string) {
	// Check for already-running nodes
	for _, name := range nodeNames {
		pid, err := readPidFile(name)
		if err == nil && pid != 0 && checkPid(pid) {
			fmt.Printf("⨯ mesh node %q is already running (pid %d).\n", name, pid)
			os.Exit(1)
		}
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

	// Load and validate all requested node configs
	allCfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		log.Error("Config load failed", "path", configPath, "error", err)
		os.Exit(1)
	}

	cfgs := make(map[string]*config.Config, len(nodeNames))
	for _, name := range nodeNames {
		cfg, ok := allCfgs[name]
		if !ok {
			log.Error("Node not found in config", "node", name, "path", configPath)
			os.Exit(1)
		}
		if err := cfg.Validate(); err != nil {
			log.Error("Config validation failed", "node", name, "error", err)
			os.Exit(1)
		}
		cfgs[name] = cfg
	}

	// Use the most verbose log level across all nodes
	logLevel := slog.LevelError
	for _, cfg := range cfgs {
		var level slog.Level
		switch strings.ToLower(cfg.LogLevel) {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		default:
			level = slog.LevelInfo
		}
		if level < logLevel {
			logLevel = level
		}
	}

	// Phase 2: Set up log destination — file (dashboard mode) or stderr (classic mode).
	ring := newLogRing(1000)
	var logFilePath string

	if useDashboard {
		home, _ := os.UserHomeDir()
		logDir := filepath.Join(home, ".mesh", "log")
		_ = os.MkdirAll(logDir, 0750)
		// Single node uses node name for log file; multi-node uses "mesh.log"
		logFileName := "mesh.log"
		if len(nodeNames) == 1 {
			logFileName = nodeNames[0] + ".log"
		}
		logFilePath = filepath.Join(logDir, logFileName)
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) //nolint:gosec // G304: logFilePath is constructed from UserHomeDir + fixed path components
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not open log file %s: %v (falling back to stderr)\n", logFilePath, err)
			useDashboard = false
			logFilePath = ""
		} else {
			defer func() { _ = logFile.Close() }()
			// Redirect runtime crash output (panics) to the log file so they
			// are not lost when running in dashboard (alternate screen) mode.
			_ = debug.SetCrashOutput(logFile, debug.CrashOptions{})
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
		ringHandler := tint.NewHandler(ring, &tint.Options{
			Level:      logLevel,
			TimeFormat: "15:04:05.000",
			NoColor:    true,
		})
		log = slog.New(&humanLogHandler{Handler: &multiHandler{plain: logHandler, color: ringHandler}})
		slog.SetDefault(log)
	}

	log.Info("mesh starting", "version", version, "nodes", strings.Join(nodeNames, ","), "config", configPath)

	// Warn about unsupported SSH options now that the proper logger is active
	// (Phase 2 routes logs to file in dashboard mode, not stderr).
	for _, cfg := range cfgs {
		config.WarnUnsupportedOptions(cfg)
	}

	// Write PID files for all nodes (same PID)
	for _, name := range nodeNames {
		if err := writePidFile(name); err != nil {
			log.Error("Failed to write pidfile", "node", name, "error", err)
		} else {
			name := name
			defer removePidFile(name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state.Global.StartEviction(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		sig := <-sigCh
		log.Info("Shutting down", "signal", sig)
		cancel()
		// Second signal forces immediate exit for stuck shutdowns.
		sig = <-sigCh
		log.Warn("Forced exit", "signal", sig)
		os.Exit(1)
	}()

	// Determine admin server address: first non-empty admin_addr across nodes wins.
	// "off" disables the admin server. Default: localhost:7777.
	adminAddr := "127.0.0.1:7777"
	for _, cfg := range cfgs {
		if cfg.AdminAddr != "" {
			adminAddr = cfg.AdminAddr
			break
		}
	}

	// Single admin HTTP endpoint — write port file for each node (same port).
	// Disabled when admin_addr is "off" in the node config.
	var adminURL string
	if adminAddr != "off" {
		adminLn, err := net.Listen("tcp", adminAddr)
		if err != nil {
			log.Warn("admin server failed to start", "addr", adminAddr, "err", err)
		} else {
			port := adminLn.Addr().(*net.TCPAddr).Port
			portStr := []byte(strconv.Itoa(port))
			adminURL = fmt.Sprintf("http://127.0.0.1:%d/ui", port)
			for _, name := range nodeNames {
				_ = os.WriteFile(portFilePath(name), portStr, 0600)
				name := name
				defer func(n string) { _ = os.Remove(portFilePath(n)) }(name)
			}

			adminSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: buildAdminMux(ring, logFilePath)}
			go func() { _ = adminSrv.Serve(adminLn) }()
			context.AfterFunc(ctx, func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = adminSrv.Shutdown(shutdownCtx)
			})
		}
	}

	go startSelfMonitor(ctx, log)

	var wg sync.WaitGroup

	// Start components for each node
	for _, nodeName := range nodeNames {
		cfg := cfgs[nodeName]

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
			nodeName := nodeName
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

		// 4. Filesync
		for _, fs := range cfg.Filesync {
			fs := fs
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := filesync.Start(ctx, fs); err != nil {
					log.Error("Filesync failed to start", "error", err)
				}
			}()
		}

		// 5. Gateway
		for _, gw := range cfg.Gateway {
			gw := gw
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := gateway.Start(ctx, gw, log); err != nil {
					log.Error("Gateway failed to start", "name", gw.Name, "error", err)
				}
			}()
		}
	}

	// 5. Live dashboard or block until signal
	if useDashboard {
		go runDashboard(ctx, cancel, cfgs, nodeNames, configPath, logFilePath, adminURL, ring)
	}

	<-ctx.Done()

	wg.Wait()
	log.Info("mesh gracefully stopped")

	// Dashboard cleanup happens via bubbletea's deferred alternate-screen exit.
	// No final status print — it pollutes the console scrollback.
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

// Package-level key classification maps — allocated once, avoids per-record map allocations.
var (
	humanLogInlineKeys = map[string]bool{
		"target": true, "bind": true, "addr": true, "peer": true,
		"remote": true, "listen": true, "name": true, "file": true, "path": true,
	}
	humanLogDetailKeys = map[string]bool{
		"tcp": true, "ssh": true, "elapsed": true, "retry_in": true,
		"version": true, "node": true, "config": true, "signal": true,
		"type": true, "set": true, "formats": true, "files": true,
		"count": true, "size": true, "user": true, "from": true,
		"status": true, "fail_count": true,
	}
)

func (h *humanLogHandler) Handle(ctx context.Context, r slog.Record) error {
	var msg strings.Builder
	msg.WriteString(r.Message)

	var details []string
	var errorVal string
	var remaining []slog.Attr

	// Single pass: classify each attr and build message parts.
	r.Attrs(func(a slog.Attr) bool {
		switch {
		case humanLogInlineKeys[a.Key]:
			msg.WriteString(" ")
			msg.WriteString(a.Value.String())
		case humanLogDetailKeys[a.Key]:
			details = append(details, a.Key+": "+a.Value.String())
		case a.Key == "error":
			errorVal = a.Value.String()
		default:
			remaining = append(remaining, a)
		}
		return true
	})

	if len(details) > 0 {
		msg.WriteString(" (")
		msg.WriteString(strings.Join(details, ", "))
		msg.WriteByte(')')
	}
	if errorVal != "" {
		msg.WriteString(": ")
		msg.WriteString(errorVal)
	}

	newRecord := slog.NewRecord(r.Time, r.Level, msg.String(), r.PC)
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
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/api/state", string(portData)))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var s map[string]state.Component
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func statusCmd(nodeNames []string, configPath string, watch bool) {
	// Check which nodes are running and collect their PIDs
	type nodeInfo struct {
		name string
		pid  int
	}
	var running []nodeInfo
	for _, name := range nodeNames {
		pid, err := readPidFile(name)
		if err != nil || pid == 0 {
			fmt.Printf("%s⨯ mesh node %q is not running.%s\n", cRed, name, cReset)
			continue
		}
		if !checkPid(pid) {
			fmt.Printf("%s⨯ mesh node %q is dead but pidfile exists (pid %d).%s\n", cRed, name, pid, cReset)
			continue
		}
		running = append(running, nodeInfo{name, pid})
	}
	if len(running) == 0 {
		os.Exit(3)
	}

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&humanLogHandler{Handler: logHandler})
	slog.SetDefault(log)

	allCfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⚠ Could not load configuration to show details: %v%s\n", cYellow, err, cReset)
		os.Exit(0)
	}

	if !watch {
		// One-shot mode
		for _, n := range running {
			cfg, ok := allCfgs[n.name]
			if !ok {
				fmt.Printf("%s⚠ Node %q not found in config%s\n", cYellow, n.name, cReset)
				continue
			}
			fmt.Printf("%s✔ mesh node %q is running (pid %d).%s\n\n", cGreen, n.name, n.pid, cReset)
			s, _ := renderStatus(cfg, fetchState(n.name), nil, n.name)
			fmt.Print(s)
		}
		os.Exit(0)
	}

	// Watch mode — alternate screen buffer, overwrite in-place
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	winch, stopWinch := winchSignal()
	defer stopWinch()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	fmt.Print("\033[?1049h\033[?25l") // alternate screen, hide cursor
	defer fmt.Print("\033[?25h\033[?1049l")

	render := func() {
		var lines []string
		for _, n := range running {
			if !checkPid(n.pid) {
				fmt.Print("\033[?25h\033[?1049l") // restore screen
				fmt.Printf("%s⨯ mesh node %q has stopped.%s\n", cRed, n.name, cReset)
				os.Exit(0)
			}
			header := fmt.Sprintf("%s✔ mesh node %q is running (pid %d)%s | %s",
				cGreen, n.name, n.pid, cReset, time.Now().Format("15:04:05"))
			lines = append(lines, header, "")
			if cfg, ok := allCfgs[n.name]; ok {
				statusOutput, _ := renderStatus(cfg, fetchState(n.name), nil, n.name)
				lines = append(lines, strings.Split(strings.TrimRight(statusOutput, "\n"), "\n")...)
			}
		}

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

func configCmd(nodeNames []string, configPath string) {
	allCfgs, err := config.LoadUnvalidated(configPath)
	if err != nil {
		fmt.Printf("%s⨯ Could not load configuration: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}

	exitCode := 0
	for _, name := range nodeNames {
		cfg, ok := allCfgs[name]
		if !ok {
			fmt.Printf("%s⨯ Node %q not found in %s%s\n\n", cRed, name, configPath, cReset)
			fmt.Printf("%sAvailable nodes:%s\n", cBold, cReset)
			for n := range allCfgs {
				fmt.Printf("  %s%s%s\n", cCyan, n, cReset)
			}
			exitCode = 1
			continue
		}
		s, _ := renderStatus(cfg, nil, nil, name)
		fmt.Print(s)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func downCmd(nodeNames []string) {
	// Deduplicate PIDs — multi-node up writes the same PID for all nodes.
	killedPids := make(map[int]bool)

	for _, name := range nodeNames {
		pid, err := readPidFile(name)
		if err != nil || pid == 0 {
			fmt.Printf("mesh node %q is not running.\n", name)
			continue
		}

		if !checkPid(pid) {
			fmt.Printf("mesh node %q is not running (stale pidfile).\n", name)
			removePidFile(name)
			continue
		}

		if killedPids[pid] {
			// Already sent SIGTERM to this PID via another node
			removePidFile(name)
			continue
		}

		fmt.Printf("Stopping mesh node %q (pid %d)...\n", name, pid)
		if err := killPid(pid, syscall.SIGTERM); err != nil {
			fmt.Printf("Error sending SIGTERM: %v\n", err)
			os.Exit(1)
		}
		killedPids[pid] = true

		// Wait for the process to actually exit (up to 10 seconds)
		stopped := false
		for i := 0; i < 100; i++ {
			if !checkPid(pid) {
				stopped = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if stopped {
			removePidFile(name)
			fmt.Println("Stopped.")
		} else {
			fmt.Println("Warning: process did not exit within 10 seconds.")
		}
	}
}

// runDir returns ~/.mesh/run, creating it if necessary.
func runDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	dir := filepath.Join(home, ".mesh", "run")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

func portFilePath(nodeName string) string {
	return filepath.Join(runDir(), fmt.Sprintf("mesh-%s.port", nodeName))
}

func pidFilePath(nodeName string) string {
	return filepath.Join(runDir(), fmt.Sprintf("mesh-%s.pid", nodeName))
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
	_ = os.Remove(pidFilePath(nodeName))
}

func checkPid(pid int) bool {
	if runtime.GOOS == "windows" {
		// FindProcess always succeeds on Windows. Instead, explicitly poll tasklist.
		cmd := exec.Command("tasklist", "/NH", "/FI", fmt.Sprintf("PID eq %d", pid)) //nolint:gosec // G204: pid is int, format string is fixed
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

    local commands="up down status config help completion"
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
            grep -E '^[a-zA-Z_][a-zA-Z0-9_-]*:' "$config_file" 2>/dev/null | sed 's/:.*//'
        fi
    }

    # Find the command position (first positional, skipping flags and their args)
    local cmd_pos=""
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
                if [[ -z "$cmd_pos" ]]; then
                    cmd_pos=$i
                fi
                break
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

    # First positional arg: command
    if [[ -z "$cmd_pos" ]]; then
        COMPREPLY=($(compgen -W "$commands" -- "$cur"))
        return
    fi

    # After "completion" command: shell names
    if [[ "${words[cmd_pos]}" == "completion" ]]; then
        COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
        return
    fi

    # After other commands: node names (repeatable)
    local nodes
    nodes=$(_mesh_nodes)
    COMPREPLY=($(compgen -W "$nodes" -- "$cur"))
}

complete -F _mesh_completions mesh
`

const completionZsh = `#compdef mesh

_mesh() {
    local -a commands=(
        'up:Start mesh nodes'
        'down:Stop running mesh nodes'
        'status:Show live status of running nodes'
        'config:Show the parsed configuration for nodes'
        'help:Show detailed help'
        'completion:Generate shell completion script'
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

    # Find command position (first positional, skipping flags and their args)
    local cmd_pos=""
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
                if [[ -z "$cmd_pos" ]]; then
                    cmd_pos=$i
                fi
                break
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

    # First positional: command
    if [[ -z "$cmd_pos" ]]; then
        _describe 'command' commands
        return
    fi

    # After "completion" command: shell names
    if [[ "${words[cmd_pos]}" == "completion" ]]; then
        compadd bash zsh fish
        return
    fi

    # After other commands: node names (repeatable)
    _mesh_nodes
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

# Check if a command has been provided (first positional, skipping flags)
function __mesh_needs_command
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
                return 1  # command already provided
        end
    end
    return 0
end

# Check if a command has been provided and it accepts node names
function __mesh_has_command_wants_nodes
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
            case up down status config
                return 0
            case '*'
                return 1
        end
    end
    return 1
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

# Commands (first positional)
complete -c mesh -n __mesh_needs_command -f -a up -d 'Start mesh nodes'
complete -c mesh -n __mesh_needs_command -f -a down -d 'Stop running mesh nodes'
complete -c mesh -n __mesh_needs_command -f -a status -d 'Show live status of running nodes'
complete -c mesh -n __mesh_needs_command -f -a config -d 'Show the parsed configuration'
complete -c mesh -n __mesh_needs_command -f -a help -d 'Show detailed help'
complete -c mesh -n __mesh_needs_command -f -a completion -d 'Generate shell completion script'

# Node names (after command, repeatable)
complete -c mesh -n __mesh_has_command_wants_nodes -f -a '(__mesh_nodes)' -d 'Node name'

# Shell names for "completion" subcommand
complete -c mesh -n __mesh_is_completion -f -a 'bash zsh fish' -d 'Shell type'
`
