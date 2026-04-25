package filesync

import (
	"path"
	"strings"
)

// builtinIgnores are always excluded from sync and scanning.
//
// `*.mesh-bak-*` carries the F7 download-protection backup written
// by installDownloadedFile (audit §6 commit 6 phase E). The contains-
// pattern matches anywhere in the basename so both `.mesh-bak-<hash>`
// (basename-prefixed style, future variants) and `<orig>.mesh-bak-<hash>`
// (the production form, where the original path is preserved as the
// prefix) are recognized as ephemeral artifacts.
var builtinIgnores = []string{
	".mesh-tmp-*",
	"*.mesh-delta-tmp-*",
	"*.mesh-bak-*",
}

// patternKind describes the fast-match strategy for a classified pattern.
type patternKind uint8

const (
	kindGeneric    patternKind = iota // fall back to path.Match
	kindLiteral                       // exact string match (no wildcards)
	kindStarSuffix                    // *.ext → strings.HasSuffix
	kindPrefixStar                    // prefix* → strings.HasPrefix
	kindContains                      // *literal* → strings.Contains
)

// linearIgnoreMatcher evaluates whether a file path should be excluded from sync.
// Patterns are pre-classified at construction time so shouldIgnore avoids
// per-call type checks and string operations (P20b).
type linearIgnoreMatcher struct {
	// builtinBase patterns match against path.Base only (no "/", no "**").
	builtinBase []classifiedPattern
	// basePatterns match against path.Base only (simple globs without "/").
	basePatterns []classifiedPattern
	// anchoredPatterns contain "/" and match against the full relative path.
	anchoredPatterns []classifiedPattern
	// doubleStarPatterns contain "**" and use the recursive matcher.
	doubleStarPatterns []classifiedPattern
}

type classifiedPattern struct {
	pattern  string
	negation bool
	dirOnly  bool
	kind     patternKind
	fixed    string // for kindLiteral: the literal; kindStarSuffix: ".ext"; kindPrefixStar: "prefix"
	// Pre-split components for double-star patterns. Populated at config
	// load time so matchDoubleStar avoids a strings.SplitN allocation per
	// path. Both fields are empty for non-double-star patterns.
	dsPrefix string // prefix before "**", with trailing "/" trimmed
	dsSuffix string // suffix after "**", with leading "/" trimmed
}

// classifyGlob determines the fast-match strategy for a simple glob pattern
// (no "/" or "**"). Returns the kind and the fixed string portion.
func classifyGlob(pattern string) (patternKind, string) {
	if strings.ContainsAny(pattern, "?[") {
		return kindGeneric, ""
	}
	n := strings.Count(pattern, "*")
	if n == 0 {
		return kindLiteral, pattern
	}
	if n == 1 {
		if strings.HasPrefix(pattern, "*") {
			return kindStarSuffix, pattern[1:] // e.g. "*.class" → ".class"
		}
		if strings.HasSuffix(pattern, "*") {
			return kindPrefixStar, pattern[:len(pattern)-1] // e.g. ".mesh-tmp-*" → ".mesh-tmp-"
		}
	}
	if n == 2 && len(pattern) >= 3 && strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		// "*literal*" — a contains-check. Middle has no further stars
		// (n==2 covers both edges) and no "?"/"[" (caught above).
		// len>=3 excludes "**"; that is handled as a double-star pattern
		// before any classifyGlob call.
		return kindContains, pattern[1 : len(pattern)-1]
	}
	return kindGeneric, ""
}

// newLinearIgnoreMatcher builds a matcher from config-level ignore patterns.
// Patterns are parsed once and pre-classified by type for fast matching.
func newLinearIgnoreMatcher(configPatterns []string) *linearIgnoreMatcher {
	m := &linearIgnoreMatcher{}

	// Built-in ignores: all are simple basename globs (no "/" or "**").
	for _, raw := range builtinIgnores {
		kind, fixed := classifyGlob(raw)
		m.builtinBase = append(m.builtinBase, classifiedPattern{
			pattern: raw, kind: kind, fixed: fixed,
		})
	}

	// Config-level patterns (gitignore-style), classified by type.
	for _, raw := range configPatterns {
		p, ok := parseLine(raw)
		if !ok {
			continue
		}
		cp := classifiedPattern{
			pattern:  p.pattern,
			negation: p.negation,
			dirOnly:  p.dirOnly,
		}
		switch {
		case strings.Contains(p.pattern, "**"):
			// Pre-split on the first "**" so matchDoubleStarPresplit avoids a
			// per-call strings.SplitN allocation (~150 allocs/op observed in
			// BenchmarkIgnoreMatcherRealistic before this change).
			if parts := strings.SplitN(p.pattern, "**", 2); len(parts) == 2 {
				cp.dsPrefix = strings.TrimSuffix(parts[0], "/")
				cp.dsSuffix = strings.TrimPrefix(parts[1], "/")
			}
			m.doubleStarPatterns = append(m.doubleStarPatterns, cp)
		case strings.Contains(p.pattern, "/"):
			cp.kind, cp.fixed = classifyGlob(p.pattern)
			m.anchoredPatterns = append(m.anchoredPatterns, cp)
		default:
			cp.kind, cp.fixed = classifyGlob(p.pattern)
			m.basePatterns = append(m.basePatterns, cp)
		}
	}

	return m
}

// parseLine parses a single ignore pattern line. Returns false for blank lines and comments.
func parseLine(line string) (classifiedPattern, bool) {
	if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
		return classifiedPattern{}, false
	}

	p := classifiedPattern{}

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
		return classifiedPattern{}, false
	}
	return p, true
}

// fastMatchBase checks whether a classified pattern matches a basename using
// compiled string ops, falling back to path.Match for complex globs.
func fastMatchBase(p *classifiedPattern, base string) bool {
	switch p.kind {
	case kindLiteral:
		return base == p.fixed
	case kindStarSuffix:
		return strings.HasSuffix(base, p.fixed)
	case kindPrefixStar:
		return strings.HasPrefix(base, p.fixed)
	case kindContains:
		return strings.Contains(base, p.fixed)
	default:
		matched, _ := path.Match(p.pattern, base)
		return matched
	}
}

// fastMatchPath checks whether a classified pattern matches a full relative
// path using compiled string ops, falling back to path.Match for complex globs.
func fastMatchPath(p *classifiedPattern, relPath string) bool {
	switch p.kind {
	case kindLiteral:
		return relPath == p.fixed
	default:
		matched, _ := path.Match(p.pattern, relPath)
		return matched
	}
}

// shouldIgnore returns true if the given relative path should be excluded.
// isDir indicates whether the path is a directory.
func (m *linearIgnoreMatcher) shouldIgnore(relPath string, isDir bool) bool {
	// Compute basename once for all basename-only patterns.
	base := path.Base(relPath)

	// H3: builtin ignores are non-negatable and checked first for early exit.
	for i := range m.builtinBase {
		if fastMatchBase(&m.builtinBase[i], base) {
			return true
		}
	}

	ignored := false

	// Basename-only patterns (no "/" or "**"). Gitignore unanchored patterns
	// match against the basename only — `path.Match(pattern, fullPath)` where
	// the pattern has no "/" cannot succeed when the path has "/" (the glob
	// `*` never crosses a separator). So fastMatchBase is sufficient; the
	// earlier fastMatchPath fallback was a no-op that paid two glob compiles
	// on every miss (PF: profiling showed it was >50% of shouldIgnore CPU).
	for i := range m.basePatterns {
		p := &m.basePatterns[i]
		if p.dirOnly && !isDir {
			continue
		}
		if fastMatchBase(p, base) {
			ignored = !p.negation
		}
	}

	// Anchored patterns (contain "/" but not "**").
	for i := range m.anchoredPatterns {
		p := &m.anchoredPatterns[i]
		if p.dirOnly && !isDir {
			continue
		}
		if fastMatchPath(p, relPath) {
			ignored = !p.negation
		}
	}

	// Double-star patterns (contain "**").
	for i := range m.doubleStarPatterns {
		p := &m.doubleStarPatterns[i]
		if p.dirOnly && !isDir {
			continue
		}
		if matchDoubleStarPresplit(p, relPath) {
			ignored = !p.negation
		}
	}

	return ignored
}

// matchPattern checks whether a gitignore-style pattern matches a relative path.
// Used by tests and fuzz targets; shouldIgnore uses the pre-classified hot path.
func matchPattern(pattern, relPath string) bool {
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, relPath)
	}
	if !strings.Contains(pattern, "/") {
		name := path.Base(relPath)
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
		if matched, _ := path.Match(pattern, relPath); matched {
			return true
		}
		return false
	}
	matched, _ := path.Match(pattern, relPath)
	return matched
}

// matchDoubleStar handles ** glob patterns. Splits on "**" per call —
// used by tests and the matchPattern fallback. The hot path goes through
// matchDoubleStarPresplit to avoid the per-call allocation.
func matchDoubleStar(pattern, filePath string) bool {
	parts := strings.SplitN(pattern, "**", 2)
	if len(parts) != 2 {
		matched, _ := path.Match(pattern, filePath)
		return matched
	}
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")
	return doubleStarMatch(prefix, suffix, filePath)
}

// matchDoubleStarPresplit uses the dsPrefix/dsSuffix fields populated at
// config load time so the hot path skips strings.SplitN.
func matchDoubleStarPresplit(p *classifiedPattern, filePath string) bool {
	return doubleStarMatch(p.dsPrefix, p.dsSuffix, filePath)
}

// doubleStarMatch is the shared ** matching logic. prefix has no trailing
// "/" and suffix has no leading "/".
func doubleStarMatch(prefix, suffix, filePath string) bool {
	if prefix != "" {
		if !strings.HasPrefix(filePath, prefix+"/") && filePath != prefix {
			return false
		}
		filePath = strings.TrimPrefix(filePath, prefix+"/")
	}
	if suffix == "" {
		return true
	}
	for {
		if matched, _ := path.Match(suffix, filePath); matched {
			return true
		}
		idx := strings.IndexByte(filePath, '/')
		if idx < 0 {
			return false
		}
		filePath = filePath[idx+1:]
	}
}

// isConflictFile returns true if the filename matches the Syncthing conflict pattern.
func isConflictFile(name string) bool {
	return strings.Contains(name, ".sync-conflict-")
}
