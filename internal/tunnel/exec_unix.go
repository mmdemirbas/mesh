//go:build !windows

package tunnel

import "os/exec"

// shellCommand returns an exec.Cmd that runs command in the platform shell.
func shellCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command) //nolint:gosec // G204: intentional — runs user-configured password_command
}
