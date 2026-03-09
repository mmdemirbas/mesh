package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/proxy"
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
	case "version", "-v", "--version":
		fmt.Printf("mesh %s\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`mesh - Connection Swiss-Army Knife

A mode-less, cross-platform networking tool acting as an all-in-one replacement 
for ssh, sshd, autossh, socat, and SOCKS/HTTP proxy servers.

Usage:
  mesh <command> [arguments]

Commands:
  up      Start mesh based on a config file
  ps      Check if mesh is running and show its active configuration
  down    Stop the currently running mesh instance
  version Print the mesh version

Examples:
  # Start mesh using a specific configuration file in the background
  mesh up -config configs/example.yml &

  # Check if the daemon is running and view configuration
  mesh ps -config configs/example.yml

  # Gracefully stop the daemon
  mesh down
`)
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

	log.Info("mesh starting", "version", version, "name", cfg.Name)

	if err := writePidFile(); err != nil {
		log.Error("Failed to write pidfile", "error", err)
	} else {
		defer removePidFile()
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

	// 1. Standalone proxies
	if len(cfg.Proxies) > 0 {
		proxy.RunStandaloneProxies(ctx, cfg.Proxies, log, &wg)
	}

	// 2. Standalone TCP Relays (e.g. sidecar usage)
	if len(cfg.Relays) > 0 {
		proxy.RunStandaloneRelays(ctx, cfg.Relays, log, &wg)
	}

	// 3. SSH servers (accept incoming connections)
	for _, srv := range cfg.Servers {
		srv := srv
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := tunnel.NewSSHServer(srv, log)
			if err := s.Run(ctx); err != nil {
				log.Error("SSH server failed", "listen", srv.Listen, "error", err)
			}
		}()
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
		os.Exit(3) // LSB status code for "program is not running"
	}

	if !checkPid(pid) {
		fmt.Printf("%s⨯ mesh is dead but pidfile exists (pid %d).%s\n", cRed, pid, cReset)
		os.Exit(1)
	}

	fmt.Printf("%s✔ mesh is running (pid %d).%s\n\n", cGreen, pid, cReset)

	cfg, err := config.LoadUnvalidated(*configPath)
	if err != nil {
		fmt.Printf("%s⚠ Could not load configuration to show details: %v%s\n", cYellow, err, cReset)
		os.Exit(0)
	}

	fmt.Printf("%s⚙ Configuration: %s%s%s\n", cBold, cCyan, cfg.Name, cReset)
	fmt.Println(cGray + strings.Repeat("─", 50) + cReset)

	pad := func(s string, w int) string {
		if len(s) < w {
			return s + strings.Repeat(" ", w-len(s))
		}
		return s
	}

	if len(cfg.Proxies) > 0 {
		fmt.Printf("%s📡 Standalone Proxies%s\n", cBold, cReset)
		for _, p := range cfg.Proxies {
			upstream := ""
			if p.Upstream != "" {
				upstream = fmt.Sprintf(" %s→%s %s", cGray, cReset, p.Upstream)
			}
			fmt.Printf("  %s%s%s %s%s%s%s\n", cBlue, pad(strings.ToUpper(p.Type), 5), cReset, cYellow, p.Bind, cReset, upstream)
		}
		fmt.Println()
	}

	if len(cfg.Relays) > 0 {
		fmt.Printf("%s🔌 Standalone Relays%s\n", cBold, cReset)
		for _, r := range cfg.Relays {
			fmt.Printf("  %s%s%s %s→%s %s%s%s\n", cYellow, pad(r.Bind, 21), cReset, cGray, cReset, cYellow, r.Target, cReset)
		}
		fmt.Println()
	}

	if len(cfg.Servers) > 0 {
		fmt.Printf("%s🛡️  SSH Servers%s\n", cBold, cReset)
		for _, s := range cfg.Servers {
			fmt.Printf("  %slisten%s %s%s%s\n", cGray, cReset, cGreen, s.Listen, cReset)
		}
		fmt.Println()
	}

	if len(cfg.Connections) > 0 {
		fmt.Printf("%s🚀 Outbound Connections%s\n", cBold, cReset)
		for _, c := range cfg.Connections {
			fmt.Printf("  %s%s%s %s(targets: %s)%s\n", cMagenta, c.Name, cReset, cGray, strings.Join(c.Targets, ", "), cReset)
			for _, fset := range c.Forwards {
				fmt.Printf("    %s[%s]%s\n", cCyan, fset.Name, cReset)
				for _, fwd := range fset.Local {
					fmt.Printf("      %s-L%s %s %s→%s %s%s%s\n", cGreen, cReset, pad(cYellow+fwd.Bind+cReset, 21+len(cYellow)+len(cReset)), cGray, cReset, cYellow, fwd.Target, cReset)
				}
				for _, fwd := range fset.Remote {
					fmt.Printf("      %s-R%s %s %s→%s %s%s%s\n", cBlue, cReset, pad(cYellow+fwd.Bind+cReset, 21+len(cYellow)+len(cReset)), cGray, cReset, cYellow, fwd.Target, cReset)
				}
				for _, pxy := range fset.Proxies.Local {
					upstream := ""
					if pxy.Upstream != "" {
						upstream = fmt.Sprintf(" %svia %s%s", cGray, pxy.Upstream, cReset)
					}
					fmt.Printf("      %s-D%s %s %s(%s)%s%s\n", cGreen, cReset, pad(cYellow+pxy.Bind+cReset, 21+len(cYellow)+len(cReset)), cGray, pxy.Type, cReset, upstream)
				}
				for _, pxy := range fset.Proxies.Remote {
					upstream := ""
					if pxy.Upstream != "" {
						upstream = fmt.Sprintf(" %svia %s%s", cGray, pxy.Upstream, cReset)
					}
					fmt.Printf("      %s-R%s %s %s(remote %s)%s%s\n", cBlue, cReset, pad(cYellow+pxy.Bind+cReset, 21+len(cYellow)+len(cReset)), cGray, pxy.Type, cReset, upstream)
				}
			}
		}
		fmt.Println()
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
