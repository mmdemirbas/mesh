package tunnel

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
