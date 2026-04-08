//go:build windows

package tunnel

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"sync"

	"golang.org/x/crypto/ssh"
)

// defaultShell returns the Windows command interpreter.
func defaultShell() string {
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
		env = append(env, "HOMEDRIVE="+home[:2])
		env = append(env, "HOMEPATH="+home[2:])
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
func handleSession(ctx context.Context, newChan ssh.NewChannel, shellCommand []string, acceptEnv []string, log *slog.Logger) {
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
			case "shell", "exec":
				cmdStart.Do(func() {
					var name string
					var args []string

					if req.Type == "exec" {
						// Run the client's command via shell /C
						var payload struct{ Command string }
						if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
							log.Warn("Failed to parse exec payload", "error", err)
							if req.WantReply {
								_ = req.Reply(false, nil)
							}
							return
						}
						name = shell
						args = []string{"/C", payload.Command}
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
					cmd.Env = append(sessionEnv(shell, "xterm"), clientEnv...)
					if home, err := os.UserHomeDir(); err == nil {
						cmd.Dir = home
					}

					err := cmd.Start()
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

			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()
}
