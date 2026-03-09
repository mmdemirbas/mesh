//go:build windows

package tunnel

import (
	"log/slog"

	"golang.org/x/crypto/ssh"
)

// handleSession handles an SSH session channel.
// On Windows, PTY allocation via creack/pty is not fully supported in the same way.
// We reject shell requests for now, or just provide a basic fallback.
func handleSession(newChan ssh.NewChannel, shellCommand []string, log *slog.Logger) {
	log.Warn("Interactive shell/PTY is currently not supported on Windows mesh servers")
	newChan.Reject(ssh.Prohibited, "interactive shell disabled on windows")
}
