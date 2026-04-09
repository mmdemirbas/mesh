//go:build windows

package tunnel

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// defaultShell returns the Windows shell, preferring modern PowerShell (pwsh.exe).
// Falls back to COMSPEC (typically cmd.exe) if pwsh is not installed.
func defaultShell() string {
	if path, err := exec.LookPath("pwsh.exe"); err == nil {
		return path
	}
	if comspec := os.Getenv("COMSPEC"); comspec != "" {
		return comspec
	}
	return "cmd.exe"
}

// sessionEnv builds environment variables for a shell session on Windows.
func sessionEnv(shell, termName string) []string {
	env := []string{
		"COMSPEC=" + shell,
		"TERM=" + termName,
	}
	if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "USERPROFILE="+home)
		vol := filepath.VolumeName(home)
		if vol != "" {
			env = append(env, "HOMEDRIVE="+vol)
			env = append(env, "HOMEPATH="+home[len(vol):])
		}
	}
	if u, err := user.Current(); err == nil {
		env = append(env, "USERNAME="+u.Username)
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
		env = append(env, "SystemRoot="+sysRoot)
	}
	return env
}

// handleSession handles an SSH session channel on Windows.
// Windows lacks Unix PTY support, but we accept pty-req anyway so that clients
// (which request a PTY by default for interactive sessions) don't see
// "PTY allocation request failed." The shell runs with plain pipes — this works
// well for cmd.exe, PowerShell, and most CLI tools. Programs that query terminal
// attributes (e.g., curses/ncurses) won't render correctly, but that's rare on Windows.
func handleSession(ctx context.Context, newChan ssh.NewChannel, shellCommand []string, acceptEnv []string, motd []byte, sftpEnabled bool, sftpRoot string, allowAgentFwd bool, sshConn ssh.Conn, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic recovered in session handler", "panic", r)
		}
	}()
	ch, reqs, err := newChan.Accept()
	if err != nil {
		log.Error("Accept session channel failed", "error", err)
		return
	}

	var (
		cmd       *exec.Cmd
		cmdStart  sync.Once
		closeOnce sync.Once
		clientEnv []string
	)
	closeCh := func() { closeOnce.Do(func() { ch.Close() }) }

	// Resolve the shell to use for interactive sessions
	shell := defaultShell()
	if len(shellCommand) > 0 {
		shell = shellCommand[0]
	}

	go func() {
		defer closeCh()
		for req := range reqs {
			switch req.Type {
			case "pty-req":
				// Accept the PTY request so clients don't error, even though
				// we use plain pipes underneath. This is the standard approach
				// for Go SSH servers on Windows without ConPTY.
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			case "window-change":
				// Acknowledge but ignore — no real PTY to resize.
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			case "signal":
				// RFC 4254 section 6.9 — Windows only supports hard kill.
				var sigReq struct{ Signal string }
				if err := ssh.Unmarshal(req.Payload, &sigReq); err != nil {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				switch sigReq.Signal {
				case "KILL", "TERM", "INT", "HUP":
					if cmd != nil && cmd.Process != nil {
						err := cmd.Process.Kill()
						if req.WantReply {
							_ = req.Reply(err == nil, nil)
						}
					} else {
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
					}
				default:
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}

			case "shell", "exec":
				cmdStart.Do(func() {
					var name string
					var args []string

					if req.Type == "exec" {
						var payload struct{ Command string }
						if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
							log.Warn("Failed to parse exec payload", "error", err)
							if req.WantReply {
								_ = req.Reply(false, nil)
							}
							return
						}
						name = shell
						base := filepath.Base(name)
						if base == "pwsh.exe" || base == "powershell.exe" {
							args = []string{"-Command", payload.Command}
						} else {
							args = []string{"/C", payload.Command}
						}
					} else {
						// Interactive shell session
						name = shell
						if len(shellCommand) > 1 {
							args = shellCommand[1:]
						}
					}

					cmd = exec.CommandContext(ctx, name, args...)
					cmd.Stdin = ch
					cmd.Stdout = ch
					cmd.Stderr = ch
					baseEnv := append(sessionEnv(shell, "xterm"), clientEnv...)
					envCopy := make([]string, len(baseEnv))
					copy(envCopy, baseEnv)
					cmd.Env = envCopy
					if home, err := os.UserHomeDir(); err == nil {
						cmd.Dir = home
					}

					// Kill process on context cancel (matches Unix cmd.Cancel behavior).
					cmd.Cancel = func() error {
						if cmd.Process != nil {
							return cmd.Process.Kill()
						}
						return nil
					}
					cmd.WaitDelay = 3 * time.Second

					// Run cmd.Start in a fresh goroutine to prevent GC stack scan
					// corruption of the long-lived session goroutine's stack during
					// syscall.StartProcess (Go runtime bug on Windows).
					startErr := make(chan error, 1)
					go func() { startErr <- cmd.Start() }()
					err := <-startErr
					if err != nil {
						log.Error("Start shell failed", "command", name, "error", err)
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
						closeCh()
						return
					}

					if req.WantReply {
						_ = req.Reply(true, nil)
					}

					if len(motd) > 0 {
						_, _ = ch.Write(motd)
					}

					go func() {
						err := cmd.Wait()
						status := uint32(0)
						if err != nil {
							if exiterr, ok := err.(*exec.ExitError); ok {
								status = uint32(exiterr.ExitCode())
							}
						}

						msg := make([]byte, 4)
						binary.BigEndian.PutUint32(msg, status)
						_, _ = ch.SendRequest("exit-status", false, msg)
						closeCh()
					}()
				})
			case "env":
				var envReq struct {
					Name  string
					Value string
				}
				if err := ssh.Unmarshal(req.Payload, &envReq); err != nil {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				if envMatches(envReq.Name, acceptEnv) {
					clientEnv = append(clientEnv, envReq.Name+"="+envReq.Value)
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
				} else {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}

			case "subsystem":
				var payload struct{ Name string }
				if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				if payload.Name != "sftp" || !sftpEnabled {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
				cmdStart.Do(func() {
					root := sftpRoot
					if root == "" {
						home, err := os.UserHomeDir()
						if err != nil {
							log.Warn("Cannot determine home directory for SFTP", "error", err)
							closeCh()
							return
						}
						root = home
					}
					server, err := sftp.NewServer(ch,
						sftp.WithServerWorkingDirectory(root),
						sftp.ReadOnly(),
					)
					if err != nil {
						log.Warn("SFTP server creation failed", "error", err)
						closeCh()
						return
					}
					log.Info("SFTP session started", "root", root)
					if err := server.Serve(); err != nil && err != io.EOF {
						log.Warn("SFTP session error", "error", err)
					}
					closeCh()
				})

			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()
}
