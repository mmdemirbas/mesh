//go:build !windows

package tunnel

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshSignalNumber maps an SSH signal name (RFC 4254 section 6.9) to a Unix signal.
func sshSignalNumber(name string) (syscall.Signal, bool) {
	switch name {
	case "ABRT":
		return syscall.SIGABRT, true
	case "ALRM":
		return syscall.SIGALRM, true
	case "FPE":
		return syscall.SIGFPE, true
	case "HUP":
		return syscall.SIGHUP, true
	case "ILL":
		return syscall.SIGILL, true
	case "INT":
		return syscall.SIGINT, true
	case "KILL":
		return syscall.SIGKILL, true
	case "PIPE":
		return syscall.SIGPIPE, true
	case "QUIT":
		return syscall.SIGQUIT, true
	case "SEGV":
		return syscall.SIGSEGV, true
	case "TERM":
		return syscall.SIGTERM, true
	case "USR1":
		return syscall.SIGUSR1, true
	case "USR2":
		return syscall.SIGUSR2, true
	default:
		return 0, false
	}
}

// signalName maps a Unix signal to its SSH signal name per RFC 4254 section 6.10.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGABRT:
		return "ABRT"
	case syscall.SIGALRM:
		return "ALRM"
	case syscall.SIGFPE:
		return "FPE"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGILL:
		return "ILL"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGPIPE:
		return "PIPE"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGSEGV:
		return "SEGV"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGUSR1:
		return "USR1"
	case syscall.SIGUSR2:
		return "USR2"
	default:
		return sig.String()
	}
}

// defaultShell returns the user's login shell or /bin/sh as fallback.
func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// sessionEnv builds environment variables for a shell session, similar to sshd.
func sessionEnv(shell, termName string) []string {
	env := []string{
		"SHELL=" + shell,
		"TERM=" + termName,
	}
	if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+home)
	}
	if u, err := user.Current(); err == nil {
		env = append(env, "USER="+u.Username)
		env = append(env, "LOGNAME="+u.Username)
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	return env
}

// handleSession handles an SSH session channel, which includes PTY allocation and shell execution.
func handleSession(ctx context.Context, newChan ssh.NewChannel, shellCommand []string, acceptEnv []string, motd []byte, sftpEnabled bool, sftpRoot string, log *slog.Logger) {
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
		ptm       *os.File // pty master
		pts       *os.File // pty slave
		cmd       *exec.Cmd
		cmdStart  sync.Once
		closeOnce sync.Once
		termName  = "xterm"    // default; updated by pty-req
		clientEnv []string     // env vars accepted from client via "env" requests
	)
	closeCh := func() { closeOnce.Do(func() { _ = ch.Close() }) }
	defer closeCh()

	defer func() {
		if ptm != nil {
			_ = ptm.Close()
		}
		if pts != nil {
			_ = pts.Close()
		}
	}()

	// Resolve the shell to use for interactive sessions
	shell := defaultShell()
	if len(shellCommand) > 0 {
		shell = shellCommand[0]
	}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if ptm != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}

			// Parse terminal dimensions (RFC 4254 Section 6.2)
			var dim struct {
				Term     string
				Cols     uint32
				Rows     uint32
				WidthPx  uint32
				HeightPx uint32
				Modes    string // encoded terminal modes (opcode-value pairs)
			}
			if err := ssh.Unmarshal(req.Payload, &dim); err != nil {
				log.Warn("Failed to parse pty-req payload", "error", err)
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}

			termName = dim.Term
			if termName == "" {
				termName = "xterm"
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

		case "signal":
			// RFC 4254 section 6.9 — deliver signal to the process group.
			var sigReq struct{ Signal string }
			if err := ssh.Unmarshal(req.Payload, &sigReq); err != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			sig, ok := sshSignalNumber(sigReq.Signal)
			if !ok || cmd == nil || cmd.Process == nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			err := syscall.Kill(-cmd.Process.Pid, sig)
			if req.WantReply {
				_ = req.Reply(err == nil, nil)
			}

		case "shell", "exec":
			cmdStart.Do(func() {
				var name string
				var args []string

				if req.Type == "exec" {
					// Run the client's command via shell -c
					var payload struct{ Command string }
					if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
						log.Warn("Failed to parse exec payload", "error", err)
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
						return
					}
					name = shell
					args = []string{"-c", payload.Command}
				} else {
					// Interactive shell session
					name = shell
					if len(shellCommand) > 1 {
						args = shellCommand[1:]
					}
				}

				// Utilize context to enforce reaping on daemon exit
				cmd = exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: intentional — launches user's login shell
				cmd.Env = append(sessionEnv(shell, termName), clientEnv...)
				if home, err := os.UserHomeDir(); err == nil {
					cmd.Dir = home
				}

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
					log.Error("Start shell failed", "command", name, "error", err)
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					closeCh()
					return
				}

				if pts != nil {
					// We started the PTY slave above, we can close it from our process now that the child has it.
					_ = pts.Close()
					pts = nil
				}

				if req.WantReply {
					_ = req.Reply(true, nil)
				}

				// Write MOTD before starting I/O relay
				if len(motd) > 0 {
					_, _ = ch.Write(motd)
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

					if err != nil {
						if exiterr, ok := err.(*exec.ExitError); ok {
							if sys, ok := exiterr.Sys().(syscall.WaitStatus); ok && sys.Signaled() {
								// Process killed by signal — send exit-signal (RFC 4254 section 6.10)
								sigName := signalName(sys.Signal())
								payload := ssh.Marshal(struct {
									Signal     string
									CoreDumped bool
									ErrMsg     string
									Lang       string
								}{sigName, sys.CoreDump(), "", ""})
								_, _ = ch.SendRequest("exit-signal", false, payload)
								closeCh()
								return
							}
						}
					}

					// Normal exit — send exit-status
					status := uint32(0)
					if err != nil {
						if exiterr, ok := err.(*exec.ExitError); ok {
							if sys, ok := exiterr.Sys().(syscall.WaitStatus); ok {
								status = uint32(sys.ExitStatus())
							}
						}
					}
					msg := make([]byte, 4)
					binary.BigEndian.PutUint32(msg, status)
					_, _ = ch.SendRequest("exit-status", false, msg)
					closeCh()
				}()
			})

		case "env":
			// RFC 4254 section 6.4 — environment variable request
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
				log.Debug("Rejected env var", "name", envReq.Name)
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
				log.Debug("Rejected subsystem request", "subsystem", payload.Name, "sftp_enabled", sftpEnabled)
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
}
