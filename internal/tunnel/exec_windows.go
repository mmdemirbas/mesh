//go:build windows

package tunnel

import (
	"os"
	"os/exec"
)

// shellCommand returns an exec.Cmd that runs command in the platform shell.
// Prefers PowerShell (pwsh.exe) if available, falls back to cmd.exe.
func shellCommand(command string) *exec.Cmd {
	if pwsh, err := exec.LookPath("pwsh.exe"); err == nil {
		return exec.Command(pwsh, "-NoProfile", "-Command", command) //nolint:gosec // G204: intentional — runs user-configured password_command
	}
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	return exec.Command(comspec, "/C", command) //nolint:gosec // G204: intentional — runs user-configured password_command
}
