//go:build windows

package tunnel

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os/exec"
	"sync"

	"golang.org/x/crypto/ssh"
)

// handleSession handles an SSH session channel on Windows.
// Windows lacks Unix PTY support, but we accept pty-req anyway so that clients
// (which request a PTY by default for interactive sessions) don't see
// "PTY allocation request failed." The shell runs with plain pipes — this works
// well for cmd.exe, PowerShell, and most CLI tools. Programs that query terminal
// attributes (e.g., curses/ncurses) won't render correctly, but that's rare on Windows.
func handleSession(ctx context.Context, newChan ssh.NewChannel, shellCommand []string, log *slog.Logger) {
	if len(shellCommand) == 0 {
		_ = newChan.Reject(ssh.Prohibited, "shell execution disabled")
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		log.Error("Accept session channel failed", "error", err)
		return
	}

	var (
		cmd      *exec.Cmd
		cmdStart sync.Once
	)

	go func() {
		defer ch.Close()
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
					name := shellCommand[0]
					var args []string
					if len(shellCommand) > 1 {
						args = shellCommand[1:]
					}

					cmd = exec.CommandContext(ctx, name, args...)
					cmd.Stdin = ch
					cmd.Stdout = ch
					cmd.Stderr = ch

					err := cmd.Start()
					if err != nil {
						log.Error("Start shell failed", "command", shellCommand, "error", err)
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
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
						ch.Close()
					}()
				})
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()
}
