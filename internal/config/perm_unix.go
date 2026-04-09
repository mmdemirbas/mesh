//go:build !windows

package config

import (
	"fmt"
	"log/slog"
	"os"
)

// warnInsecurePermissions logs a warning if the config file is writable by
// group or others. Writable configs are dangerous because they can be
// tampered with (e.g. password_command injection). Readable-by-others is
// acceptable since secrets are stored externally via password_command.
func warnInsecurePermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	mode := info.Mode().Perm()
	if mode&0022 != 0 {
		slog.Warn("Config file is writable by group/others; consider chmod 644 or 600", "path", path, "mode", fmt.Sprintf("%04o", mode))
	}
}
