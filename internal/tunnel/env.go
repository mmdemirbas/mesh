package tunnel

import (
	"fmt"
	"io"
	"os"
)

// Caps on client-supplied environment variables. RFC 4254 does not
// bound the number of "env" requests per session, so an authenticated
// peer could flood the accumulator before issuing "shell" or "exec".
// The slice is copied verbatim into cmd.Env, so unbounded growth
// translates directly into per-session memory pressure.
const (
	maxAcceptedEnvVars = 128  // total entries permitted per session
	maxEnvValueSize    = 8192 // per-value byte cap (name not included)
)

// maxBannerSize caps the on-disk size of the banner and MOTD files. The
// SSH banner is sent on every pre-auth connection, so a runaway file
// path pointed at a log or data file would amplify into massive
// bandwidth per client on top of the memory cost at startup.
const maxBannerSize = 64 * 1024

// readFileCapped reads up to limit+1 bytes from path and returns an
// error if the file exceeds limit. Used for config-sourced files whose
// runaway size would be a DoS surface.
func readFileCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path from user config, validated at load time
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file %q exceeds %d-byte cap", path, limit)
	}
	return data, nil
}

// envMatches returns true if name matches any pattern in allowlist.
// Patterns support trailing wildcard (e.g., "LC_*" matches "LC_ALL").
func envMatches(name string, allowlist []string) bool {
	for _, pat := range allowlist {
		if pat == name {
			return true
		}
		if len(pat) > 0 && pat[len(pat)-1] == '*' {
			prefix := pat[:len(pat)-1]
			if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
				return true
			}
		}
	}
	return false
}

// acceptEnvRequest decides whether a client-supplied env var should be
// admitted into the per-session accumulator. It enforces the allowlist
// match, the per-value size cap, and the total-count cap. Returns the
// concrete reason on rejection so the caller can log a single point of
// truth.
func acceptEnvRequest(currentCount int, name, value string, allowlist []string) (ok bool, reason string) {
	if currentCount >= maxAcceptedEnvVars {
		return false, "env var count cap reached"
	}
	if len(value) > maxEnvValueSize {
		return false, "env var value exceeds size cap"
	}
	if !envMatches(name, allowlist) {
		return false, "env var not in allowlist"
	}
	return true, ""
}
