//go:build !windows

package tunnel

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// handleSession handles an SSH session channel, which includes PTY allocation and shell execution.
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
	defer ch.Close()

	var (
		ptm      *os.File // pty master
		pts      *os.File // pty slave
		cmd      *exec.Cmd
		cmdStart sync.Once
	)

	defer func() {
		if ptm != nil {
			ptm.Close()
		}
		if pts != nil {
			pts.Close()
		}
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if ptm != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}

			// Parse terminal dimensions
			var dim struct {
				Term     string
				Cols     uint32
				Rows     uint32
				WidthPx  uint32
				HeightPx uint32
			}
			if err := ssh.Unmarshal(req.Payload, &dim); err != nil {
				log.Warn("Failed to parse pty-req payload", "error", err)
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}

			var err error
			ptm, pts, err = pty.Open()
			if err != nil {
				log.Error("Allocate PTY failed", "error", err)
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				return
			}

			// Set terminal size
			_ = pty.Setsize(ptm, &pty.Winsize{
				Rows: uint16(dim.Rows),
				Cols: uint16(dim.Cols),
				X:    uint16(dim.WidthPx),
				Y:    uint16(dim.HeightPx),
			})

			if req.WantReply {
				_ = req.Reply(true, nil)
			}

		case "window-change":
			if ptm == nil {
				continue
			}
			var dim struct {
				Cols     uint32
				Rows     uint32
				WidthPx  uint32
				HeightPx uint32
			}
			if err := ssh.Unmarshal(req.Payload, &dim); err != nil {
				log.Warn("Failed to parse window-change payload", "error", err)
				continue
			}
			_ = pty.Setsize(ptm, &pty.Winsize{
				Rows: uint16(dim.Rows),
				Cols: uint16(dim.Cols),
				X:    uint16(dim.WidthPx),
				Y:    uint16(dim.HeightPx),
			})

		case "shell", "exec":
			cmdStart.Do(func() {
				// Only use the configured shell command, ignore client's requested exec payload for security.
				name := shellCommand[0]
				var args []string
				if len(shellCommand) > 1 {
					args = shellCommand[1:]
				}

				// Utilize context to enforce reaping on daemon exit
				cmd = exec.CommandContext(ctx, name, args...)

				if pts != nil {
					cmd.Stdin = pts
					cmd.Stdout = pts
					cmd.Stderr = pts
					// Set controlling terminal and unique process group
					cmd.SysProcAttr = &syscall.SysProcAttr{
						Setctty: true,
						Setsid:  true,
					}
				} else {
					cmd.Stdin = ch
					cmd.Stdout = ch
					cmd.Stderr = ch
					cmd.SysProcAttr = &syscall.SysProcAttr{
						Setpgid: true, // Assign process group even without PTY
					}
				}

				// Broad target SIGKILL to the entire process group if context is canceled
				cmd.Cancel = func() error {
					if cmd.Process != nil {
						return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					return nil
				}

				err := cmd.Start()
				if err != nil {
					log.Error("Start shell failed", "command", shellCommand, "error", err)
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					ch.Close()
					return
				}

				if pts != nil {
					// We started the PTY slave above, we can close it from our process now that the child has it.
					pts.Close()
					pts = nil
				}

				if req.WantReply {
					_ = req.Reply(true, nil)
				}

				go func() {
					var wg sync.WaitGroup
					if ptm != nil {
						wg.Add(2)
						go func() {
							defer wg.Done()
							_, _ = io.Copy(ch, ptm)
						}()
						go func() {
							defer wg.Done()
							_, _ = io.Copy(ptm, ch)
						}()
					}
					wg.Wait()

					err := cmd.Wait()
					status := uint32(0)
					if err != nil {
						if exiterr, ok := err.(*exec.ExitError); ok {
							if sys, ok := exiterr.Sys().(syscall.WaitStatus); ok {
								status = uint32(sys.ExitStatus())
							}
						}
					}

					// Send exit-status
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
}
