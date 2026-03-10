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
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "up":
		upCmd()
	case "ps":
		psCmd()
	case "down":
		downCmd()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	const (
		reset  = "\033[0m"
		bold   = "\033[1m"
		blue   = "\033[34m"
		cyan   = "\033[36m"
		gray   = "\033[90m"
		yellow = "\033[33m"
	)
	fmt.Println(bold + cyan + "mesh" + reset + " " + gray + version + reset + " - Human-friendly networking tool")
	fmt.Println()
	fmt.Println("All-in-one replacement for ssh, sshd, autossh, socat, and SOCKS/HTTP proxy servers.")
	fmt.Println()
	fmt.Println(bold + "Usage:" + reset)
	fmt.Println("  mesh " + cyan + "<command>" + reset + " [arguments]")
	fmt.Println()
	fmt.Println(bold + "Commands:" + reset)
	fmt.Println("  " + blue + "up" + reset + "   Start mesh based on a config file")
	fmt.Println("  " + blue + "down" + reset + " Stop the currently running mesh instance")
	fmt.Println("  " + blue + "ps" + reset + "   Check if mesh is running and show its active configuration")
	fmt.Println()
	fmt.Println(bold + "Examples:" + reset)
	fmt.Println("  " + gray + "# Start mesh using a specific configuration file in the background" + reset)
	fmt.Println("  mesh " + blue + "up" + reset + " " + yellow + "-config" + reset + " configs/example.yml &")
	fmt.Println()
	fmt.Println("  " + gray + "# Gracefully stop the daemon" + reset)
	fmt.Println("  mesh " + blue + "down" + reset)
	fmt.Println()
	fmt.Println("  " + gray + "# Check if the daemon is running and view configuration" + reset)
	fmt.Println("  mesh " + blue + "ps" + reset + " " + yellow + "-config" + reset + " configs/example.yml")
	fmt.Println()
}

func upCmd() {
	serveFS := flag.NewFlagSet("up", flag.ExitOnError)
	configPath := serveFS.String("config", "mesh.yml", "Path to config file")
	serveFS.Parse(os.Args[2:])

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("Config load failed", "path", *configPath, "error", err)
		os.Exit(1)
	}

	var logLevel slog.Level
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	logHandler = tint.NewHandler(os.Stderr, &tint.Options{
		Level:      logLevel,
		TimeFormat: "15:04:05.000",
	})
	log = slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	log.Info("mesh starting", "version", version, "name", cfg.Name)

	if err := writePidFile(); err != nil {
		log.Error("Failed to write pidfile", "error", err)
	} else {
		defer removePidFile()
	}

	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		port := adminLn.Addr().(*net.TCPAddr).Port
		os.WriteFile(portFilePath(), []byte(strconv.Itoa(port)), 0644)
		defer os.Remove(portFilePath())

		go http.Serve(adminLn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(state.Global.Snapshot())
		}))
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

	// 4. Outbound connections (Multi-set forwards)
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

	// 5. Clipsync
	for _, cs := range cfg.Clipsync {
		cs := cs
		wg.Add(1)
		go func() {
			defer wg.Done()
			// TODO: We could need to pass and use ctx in the clipsync.Start func
			_, err := clipsync.Start(cs)
			if err != nil {
				log.Error("Clipsync failed to start", "error", err)
			}
		}()
	}

	// Block until a signal triggers context cancellation
	<-ctx.Done()

	// Wait a moment for graceful shutdown of spawned servers/clients
	wg.Wait()
	log.Info("mesh gracefully stopped")
}

type padMessageHandler struct {
	slog.Handler
	width int
}

func (h *padMessageHandler) Handle(ctx context.Context, r slog.Record) error {
	if len(r.Message) < h.width {
		r.Message += strings.Repeat(" ", h.width-len(r.Message))
	}
	return h.Handler.Handle(ctx, r)
}

func psCmd() {
	psFS := flag.NewFlagSet("ps", flag.ExitOnError)
	configPath := psFS.String("config", "mesh.yml", "Path to config file")
	psFS.Parse(os.Args[2:])

	const (
		cReset   = "\033[0m"
		cBold    = "\033[1m"
		cRed     = "\033[31m"
		cGreen   = "\033[32m"
		cYellow  = "\033[33m"
		cBlue    = "\033[34m"
		cMagenta = "\033[35m"
		cCyan    = "\033[36m"
		cGray    = "\033[90m"
	)

	pid, err := readPidFile()
	if err != nil || pid == 0 {
		fmt.Printf("%s⨯ mesh is not running.%s\n", cRed, cReset)
		os.Exit(3)
	}

	if !checkPid(pid) {
		fmt.Printf("%s⨯ mesh is dead but pidfile exists (pid %d).%s\n", cRed, pid, cReset)
		os.Exit(1)
	}

	fmt.Printf("%s✔ mesh is running (pid %d).%s\n\n", cGreen, pid, cReset)

	var activeState map[string]state.Component
	if portData, err := os.ReadFile(portFilePath()); err == nil {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/", string(portData))); err == nil {
			defer resp.Body.Close()
			json.NewDecoder(resp.Body).Decode(&activeState)
		}
	}

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

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(&padMessageHandler{Handler: logHandler, width: 30})
	slog.SetDefault(log)

	cfg, err := config.LoadUnvalidated(*configPath)
	if err != nil {
		fmt.Printf("%s⚠ Could not load configuration to show details: %v%s\n", cYellow, err, cReset)
		os.Exit(0)
	}

	fmt.Printf("%s⚙ Configuration: %s%s%s\n", cBold, cCyan, cfg.Name, cReset)
	fmt.Println(cGray + strings.Repeat("─", 80) + cReset)

	stripANSI := func(str string) string {
		return regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(str, "")
	}

	visibleLen := func(str string) int {
		return utf8.RuneCountInString(stripANSI(str))
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

	// padForProto pads a colored address so protocol labels start at the same column.
	padForProto := func(colored string) string {
		if pad := maxProtoAddr - visibleLen(colored); pad > 0 {
			return colored + strings.Repeat(" ", pad)
		}
		return colored
	}

	if len(cfg.Listeners) > 0 {
		for _, l := range cfg.Listeners {
			indicator, st, _ := getComponentInfo(l.Type, l.Bind)
			if l.Type == "sshd" {
				indicator, st, _ = getComponentInfo("server", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				addRow("", indicator, left, "", "", st)
			} else if l.Type == "relay" {
				indicator, st, _ = getComponentInfo("relay", l.Bind)
				left := colorAddr(l.Bind)
				arrow := arrowRight
				right := colorAddr(l.Target)
				addRow("", indicator, left, arrow, right, st)
			} else {
				// Proxy
				indicator, st, _ = getComponentInfo("proxy", l.Bind)
				left := padForProto(colorAddr(l.Bind)) + " " + cBlue + strings.ToLower(l.Type) + cReset
				arrow := arrowRight
				right := ""
				if l.Target != "" {
					right = colorAddr(l.Target)
				} else {
					right = cGray + "direct" + cReset
				}
				addRow("", indicator, left, arrow, right, st)
			}
		}
		addHeader("")
	}

	if len(cfg.Connections) > 0 {
		for _, c := range cfg.Connections {
			addHeader(fmt.Sprintf("%s%s%s", cMagenta, c.Name, cReset))

			connectedTargets := make(map[string]struct{})
			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				_, _, comp := getComponentInfo("connection", id)
				if (comp.Status == state.Connected || comp.Status == state.Connecting) && comp.Message != "" {
					connectedTargets[comp.Message] = struct{}{}
				}
			}

			for _, t := range c.Targets {
				ind := "◽️"
				if _, ok := connectedTargets[t]; ok {
					ind = "🔹"
				}
				addRow("  ", ind, colorAddr(t), "", "", "")
			}

			if len(c.Forwards) > 0 {
				addHeader("")
			}

			for _, fset := range c.Forwards {
				id := c.Name + " [" + fset.Name + "]"
				indicator, st, _ := getComponentInfo("connection", id)

				left := cBold + cBlue + "[" + fset.Name + "]" + cReset
				addRow("  ", indicator, left, "", "", st)

				indent := "     "

				for _, fwd := range fset.Local {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					indicator, st, comp := getComponentInfo("forward", compID)

					lStr := colorAddr(fwd.Bind)
					if comp.BoundAddr != "" && comp.BoundAddr != fwd.Bind {
						lStr = colorAddr(comp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}

					if fwd.Type == "forward" {
						rStr := colorAddr(fwd.Target)
						addRow(indent, indicator, lStr, arrowRight, rStr, st)
					} else { // socks, http
						lStr = padForProto(lStr) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						rStr := ""
						if fwd.Target != "" {
							rStr = colorAddr(fwd.Target)
						} else {
							rStr = cGray + "tunnel" + cReset
						}
						addRow(indent, indicator, lStr, arrowRight, rStr, st)
					}
				}
				for _, fwd := range fset.Remote {
					compID := fmt.Sprintf("%s [%s] %s", c.Name, fset.Name, fwd.Bind)
					indicator, st, comp := getComponentInfo("forward", compID)

					rStr := colorAddr(fwd.Bind)
					if comp.BoundAddr != "" && comp.BoundAddr != fwd.Bind {
						rStr += colorAddr(comp.BoundAddr) + " " + cGray + "(from " + fwd.Bind + ")" + cReset
					}

					if fwd.Type == "forward" {
						lStr := colorAddr(fwd.Target)
						rStr := colorAddr(fwd.Bind)
						addRow(indent, indicator, lStr, arrowLeft, rStr, st)
					} else { // socks, http
						lStr := ""
						if fwd.Target != "" {
							lStr = colorAddr(fwd.Target)
						} else {
							lStr = cGray + "tunnel" + cReset
						}
						rStr := padForProto(colorAddr(fwd.Bind)) + " " + cBlue + strings.ToLower(fwd.Type) + cReset
						addRow(indent, indicator, lStr, arrowLeft, rStr, st)
					}
				}
			}
		}
		addHeader("")
	}

	if len(cfg.Clipsync) > 0 {
		addHeader(cCyan + "clipsync" + cReset)
		for _, cs := range cfg.Clipsync {
			indicator, st, comp := getComponentInfo("clipsync", cs.Bind)

			left := colorAddr(cs.Bind)
			right := cGray + "[discovery: disabled]" + cReset
			if cs.LANDiscovery {
				right = cGray + "[discovery: active]" + cReset
			}
			if comp.Message != "" {
				right = cGray + "[" + comp.Message + "]" + cReset
			}

			if len(cs.StaticPeers) > 0 {
				right += " " + cMagenta + fmt.Sprintf("+%d static peers", len(cs.StaticPeers)) + cReset
			}

			addRow("  ", indicator, left, "", right, st)
		}
		addHeader("")
	}

	var dynamicForwards []state.Component
	for k, comp := range activeState {
		if strings.HasPrefix(k, "dynamic:") {
			dynamicForwards = append(dynamicForwards, comp)
		}
	}

	if len(dynamicForwards) > 0 {
		sort.Slice(dynamicForwards, func(i, j int) bool {
			return dynamicForwards[i].ID < dynamicForwards[j].ID
		})

		addHeader(cMagenta + "dynamic ports (peer forwards)" + cReset)
		for _, comp := range dynamicForwards {
			indicator, st, _ := getComponentInfo("dynamic", comp.ID)
			left := colorAddr(comp.ID)
			right := cGray + "tunnel (" + comp.Message + ")" + cReset
			addRow("  ", indicator, left, arrowRight, right, st)
		}
		addHeader("")
	}

	maxTotalLeft := 0
	for _, r := range rows {
		if !r.isHeader && r.left != "" && r.arrow != "" {
			l := len(r.indent)
			if r.indicator != "" {
				l += 2 // '⚪ ' indicator plus space
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

			l := visibleLen(line)
			if l > maxLineLen {
				maxLineLen = l
			}
		}
	}

	statusPadCol := maxLineLen + 1

	// Don't pad out further than 60 characters so short rows don't push statuses too far out
	if statusPadCol > 60 {
		statusPadCol = 60
	}

	for _, r := range rows {
		if r.isHeader {
			fmt.Println(r.text)
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
		fmt.Println(strings.TrimRight(line, " "))
	}

	os.Exit(0)
}

func downCmd() {
	pid, err := readPidFile()
	if err != nil || pid == 0 {
		fmt.Println("mesh is not running.")
		return
	}

	if !checkPid(pid) {
		fmt.Println("mesh is not running (stale pidfile).")
		removePidFile()
		return
	}

	fmt.Printf("Stopping mesh (pid %d)...\n", pid)
	if err := killPid(pid, syscall.SIGTERM); err != nil {
		fmt.Printf("Error sending SIGTERM: %v\n", err)
		os.Exit(1)
	}
	// Wait for the process to actually exit (up to 10 seconds)
	for i := 0; i < 100; i++ {
		if !checkPid(pid) {
			removePidFile()
			fmt.Println("Stopped.")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("Warning: process did not exit within 10 seconds.")
}

func portFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", "mesh.port")
}

func pidFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir, err = os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
	}
	os.MkdirAll(filepath.Join(dir, "mesh"), 0700)
	return filepath.Join(dir, "mesh", "mesh.pid")
}

func writePidFile() error {
	pid := os.Getpid()
	data := []byte(strconv.Itoa(pid))
	return os.WriteFile(pidFilePath(), data, 0644)
}

func readPidFile() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func removePidFile() {
	os.Remove(pidFilePath())
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
