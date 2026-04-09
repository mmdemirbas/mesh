//go:build !windows

package config

import (
	"fmt"
	"os"
)

// checkInsecurePermissions returns an error if the config file is writable by
// group or others. Writable configs are dangerous because password_command
// could be tampered with to execute arbitrary code.
func checkInsecurePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // file may not exist yet during tests
	}
	mode := info.Mode().Perm()
	if mode&0022 != 0 {
		return fmt.Errorf("config file %s has insecure permissions %04o (writable by group/others); fix with: chmod 644 %s", path, mode, path)
	}
	return nil
}
