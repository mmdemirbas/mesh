package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// checkPid returns true if the given PID is still running.
// On Windows, FindProcess always succeeds, so we poll tasklist instead.
func checkPid(pid int) bool {
	cmd := exec.Command("tasklist", "/NH", "/FI", fmt.Sprintf("PID eq %d", pid)) //nolint:gosec // G204: pid is int, format string is fixed
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), strconv.Itoa(pid))
}

// killPid sends the given signal to a process. On Windows, falls back to
// Kill() since most Unix signals are not supported.
func killPid(pid int, _ syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
