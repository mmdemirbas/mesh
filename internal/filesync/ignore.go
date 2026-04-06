package filesync

import (
	"path/filepath"
	"strings"
)

// builtinIgnores are always excluded from sync and scanning.
var builtinIgnores = []string{
	".mesh-tmp-*",
}

// ignoreMatcher evaluates whether a file path should be excluded from sync.
type ignoreMatcher struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	pattern  string
	negation bool // leading ! negates the match
	dirOnly  bool // trailing / means only match directories
}

// newIgnoreMatcher builds a matcher from config-level ignore patterns.
func newIgnoreMatcher(configPatterns []string) *ignoreMatcher {
	var patterns []ignorePattern

	// Built-in ignores first (highest priority, non-negatable).
	for _, p := range builtinIgnores {
		patterns = append(patterns, ignorePattern{pattern: p})
	}

	// Config-level patterns (gitignore-style).
	for _, raw := range configPatterns {
		if p, ok := parseLine(raw); ok {
			patterns = append(patterns, p)
		}
	}

	return &ignoreMatcher{patterns: patterns}
}

// parseLine parses a single ignore pattern line. Returns false for blank lines and comments.
func parseLine(line string) (ignorePattern, bool) {
	if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
		return ignorePattern{}, false
	}

	p := ignorePattern{}

	if strings.HasPrefix(line, "!") {
		p.negation = true
		line = line[1:]
	}

	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	p.pattern = line
	if p.pattern == "" {
		return ignorePattern{}, false
	}
	return p, true
}

// shouldIgnore returns true if the given relative path should be excluded.
// isDir indicates whether the path is a directory.
func (m *ignoreMatcher) shouldIgnore(relPath string, isDir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if matchPattern(p.pattern, relPath) {
			ignored = !p.negation
		}
	}
	return ignored
}

// matchPattern checks whether a gitignore-style pattern matches a relative path.
// Supports:
//   - Simple names ("foo") match anywhere in the tree
//   - Paths with "/" match from the root ("src/foo")
//   - Glob wildcards (*, ?)
//   - Double-star (**) matches zero or more directories
func matchPattern(pattern, relPath string) bool {
	// Patterns without "/" match the basename at any depth.
	if !strings.Contains(pattern, "/") {
		// Match against each path component.
		name := filepath.Base(relPath)
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
		// Also try matching against the full relative path for wildcard patterns.
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
		return false
	}

	// Handle ** (double-star) by expanding to match zero or more path segments.
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, relPath)
	}

	// Pattern with "/" is anchored to the root.
	matched, _ := filepath.Match(pattern, relPath)
	return matched
}

// matchDoubleStar handles ** glob patterns.
func matchDoubleStar(pattern, path string) bool {
	// Split on ** and try to match prefix + suffix with any number of middle segments.
	parts := strings.SplitN(pattern, "**", 2)
	if len(parts) != 2 {
		matched, _ := filepath.Match(pattern, path)
		return matched
	}

	prefix := parts[0]
	suffix := strings.TrimPrefix(parts[1], "/")

	// Prefix must match the start of the path.
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/")
		if !strings.HasPrefix(path, prefix+"/") && path != prefix {
			return false
		}
		path = strings.TrimPrefix(path, prefix+"/")
	}

	// Suffix must match the end (or some tail) of the remaining path.
	if suffix == "" {
		return true
	}

	// Try matching suffix against the path and all sub-paths.
	for {
		if matched, _ := filepath.Match(suffix, path); matched {
			return true
		}
		idx := strings.IndexByte(path, '/')
		if idx < 0 {
			return false
		}
		path = path[idx+1:]
	}
}

// isConflictFile returns true if the filename matches the Syncthing conflict pattern.
func isConflictFile(name string) bool {
	return strings.Contains(name, ".sync-conflict-")
}
