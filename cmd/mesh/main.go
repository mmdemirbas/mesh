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
	case "serve":
		serveCmd()
	case "status":
		statusCmd()
	case "stop":
		stopCmd()
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
  serve   Start mesh based on a config file
  status  Check if a mesh instance is currently running
  stop    Stop the currently running mesh instance
  version Print the mesh version

Examples:
  # Start mesh using a specific configuration file in the background
  mesh serve -config configs/example.yml &

  # Check if the daemon is running
  mesh status

  # Gracefully stop the daemon
  mesh stop
`)
}

func serveCmd() {
	serveFS := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := serveFS.String("config", "mesh.yml", "Path to config file")
	serveFS.Parse(os.Args[2:])

	logHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: "15:04:05.000",
	})
	log := slog.New(logHandler)

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
	log = slog.New(logHandler)

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

func statusCmd() {
	pid, err := readPidFile()
	if err != nil || pid == 0 {
		fmt.Println("mesh is not running.")
		os.Exit(3) // LSB status code for "program is not running"
	}

	if checkPid(pid) {
		fmt.Printf("mesh is running (pid %d).\n", pid)
		os.Exit(0)
	}

	fmt.Printf("mesh is dead but pidfile exists (pid %d).\n", pid)
	os.Exit(1)
}

func stopCmd() {
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
