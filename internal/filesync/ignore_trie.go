package filesync

import (
	"path"
	"strings"
)

// trieIgnoreMatcher is the PF Phase 2 parallel implementation. It preserves
// the exact semantics of ignoreMatcher (builtins short-circuit; base →
// anchored → doublestar with last-match-wins within each group) but
// replaces per-call linear scans with structure-bucketed lookups.
//
// Production still uses newIgnoreMatcher. This type exists for the
// PF Phase 2 conformance gate and benchmarks; the merge gate is zero
// divergence from linearFactory across the behavior corpus.
//
// Shape of the three groups:
//
//   - Base (pattern has neither "/" nor "**"): classified by kind.
//   - kindLiteral → map[basename][]trieRec         O(1) lookup
//   - kindStarSuffix / kindPrefixStar / kindGeneric → bucketed slices
//   - Anchored (has "/", no "**"): classified by whether any segment
//     contains a wildcard.
//   - pure-literal path → map[fullpath][]trieRec   O(1) lookup
//   - anything-else → segment trie with literal children + glob edges
//   - Double-star (has "**"): bucketed slice with pre-split prefix/suffix.
//
// Each match collects the highest-ordinal hit within its group and then
// picks the group with the highest priority (base < anchored < doublestar,
// matching the linear iteration order). That record's negation bit drives
// the final ignored decision.
type trieIgnoreMatcher struct {
	builtinBase []trieRec

	litBase map[string][]trieRec
	// extSuffix buckets kindStarSuffix patterns whose fixed part starts
	// with "." — i.e. extensions. Keyed by the extension without the
	// leading dot (e.g. "class" for "*.class"). Match-time extracts the
	// path's extension once and does a single map lookup instead of
	// iterating every star-suffix pattern. This is the dominant
	// optimization against the linear matcher.
	extSuffix     map[string][]trieRec
	otherSuffix   []trieRec // non-extension kindStarSuffix (rare: *~ etc.)
	prefixStar    []trieRec
	genericBase   []trieRec
	hasSuffixBuck bool // any extSuffix / otherSuffix entry exists

	litAnchored  map[string][]trieRec
	anchoredRoot *segNode // nil if no glob-anchored patterns

	doubleStar []trieRec

	// Skip flags so shouldIgnore avoids touching empty groups. Saves
	// a map lookup, a slice range, and their associated branch costs on
	// every call, which adds up across 310k files per scan.
	hasLitBase      bool
	hasLitAnchored  bool
	hasDoubleStar   bool
	hasAnchoredGlob bool
}

// trieRec holds the per-pattern data the matcher needs at decision time.
// ordinal is the pattern's registration index within its group; higher
// wins when multiple patterns in the same group match a path.
type trieRec struct {
	pattern  string
	negation bool
	dirOnly  bool
	kind     patternKind
	fixed    string
	dsPrefix string
	dsSuffix string
	ordinal  int
	// segGlobs holds per-segment globs for an anchored pattern that lives
	// in anchoredRoot. Empty for litAnchored / base / doublestar records.
	segGlobs []segGlob
	// dsShape classifies the double-star pattern into a fast-path shape.
	// Populated only for doubleStar records.
	dsShape     dsShape
	dsBasenameK patternKind // sub-kind for dsShapeBasename
	dsBasenameF string      // fixed portion for dsShapeBasename
}

// dsShape is a pre-classified double-star pattern shape. Generic falls
// back to doubleStarMatch; the other shapes dispatch to a specialized
// branch that avoids path.Match entirely.
type dsShape uint8

const (
	dsShapeGeneric    dsShape = iota // fall back to doubleStarMatch
	dsShapeBasename                  // dsPrefix + "**" + "/" + basename-pattern (suffix has no "/")
	dsShapeMidLiteral                // "**/LITERAL/**" — second-to-last segment equals LITERAL
	dsShapePrefixOnly                // "prefix/**" — path starts with prefix or equals it
)

// segGlob represents one segment of an anchored pattern that contains a
// wildcard. Populated at construction so match-time avoids parsing.
type segGlob struct {
	raw   string // the full segment, e.g. "*.go"
	kind  patternKind
	fixed string
}

// segNode is a node in the anchored-glob segment trie. Literal segments
// walk through `literal`; glob segments fan out through `globs` which
// holds child nodes guarded by a per-segment matcher.
type segNode struct {
	literal map[string]*segNode
	globs   []*segEdge
	// terminals fires when a path walk ends exactly at this node and the
	// path length equals the pattern's segment count.
	terminals []trieRec
}

type segEdge struct {
	seg   segGlob
	child *segNode
}

// newTrieIgnoreMatcher builds the parallel matcher. Patterns are parsed
// identically to newIgnoreMatcher so group membership is identical; only
// the per-group storage differs.
func newTrieIgnoreMatcher(configPatterns []string) *trieIgnoreMatcher {
	m := &trieIgnoreMatcher{
		litBase:     map[string][]trieRec{},
		extSuffix:   map[string][]trieRec{},
		litAnchored: map[string][]trieRec{},
	}

	for _, raw := range builtinIgnores {
		kind, fixed := classifyGlob(raw)
		m.builtinBase = append(m.builtinBase, trieRec{
			pattern: raw, kind: kind, fixed: fixed,
		})
	}

	// Per-group ordinals: incremented within each group so we can pick
	// the highest-ordinal match per group at decision time.
	var baseOrd, anchoredOrd, doubleOrd int

	for _, raw := range configPatterns {
		p, ok := parseLine(raw)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(p.pattern, "**"):
			r := trieRec{
				pattern:  p.pattern,
				negation: p.negation,
				dirOnly:  p.dirOnly,
				ordinal:  doubleOrd,
			}
			if parts := strings.SplitN(p.pattern, "**", 2); len(parts) == 2 {
				r.dsPrefix = strings.TrimSuffix(parts[0], "/")
				r.dsSuffix = strings.TrimPrefix(parts[1], "/")
			}
			classifyDoubleStar(&r)
			m.doubleStar = append(m.doubleStar, r)
			doubleOrd++
		case strings.Contains(p.pattern, "/"):
			r := trieRec{
				pattern:  p.pattern,
				negation: p.negation,
				dirOnly:  p.dirOnly,
				ordinal:  anchoredOrd,
			}
			r.kind, r.fixed = classifyGlob(p.pattern)
			if r.kind == kindLiteral {
				m.litAnchored[p.pattern] = append(m.litAnchored[p.pattern], r)
				m.hasLitAnchored = true
			} else {
				r.segGlobs = classifySegments(p.pattern)
				if m.anchoredRoot == nil {
					m.anchoredRoot = &segNode{literal: map[string]*segNode{}}
				}
				insertSegTrie(m.anchoredRoot, r)
				m.hasAnchoredGlob = true
			}
			anchoredOrd++
		default:
			r := trieRec{
				pattern:  p.pattern,
				negation: p.negation,
				dirOnly:  p.dirOnly,
				ordinal:  baseOrd,
			}
			r.kind, r.fixed = classifyGlob(p.pattern)
			switch r.kind {
			case kindLiteral:
				m.litBase[p.pattern] = append(m.litBase[p.pattern], r)
				m.hasLitBase = true
			case kindStarSuffix:
				// "*.ext" patterns (fixed=".ext") are bucketed by the
				// extension so a basename ending in ".ext" can find
				// its candidates via one map lookup. Non-extension
				// suffixes (e.g. "*~" → fixed="~") stay in otherSuffix
				// and still iterate linearly, but they are rare.
				if len(r.fixed) >= 2 && r.fixed[0] == '.' && !strings.ContainsAny(r.fixed[1:], ".") {
					ext := r.fixed[1:]
					m.extSuffix[ext] = append(m.extSuffix[ext], r)
				} else {
					m.otherSuffix = append(m.otherSuffix, r)
				}
				m.hasSuffixBuck = true
			case kindPrefixStar:
				m.prefixStar = append(m.prefixStar, r)
			default:
				m.genericBase = append(m.genericBase, r)
			}
			baseOrd++
		}
	}

	if len(m.doubleStar) > 0 {
		m.hasDoubleStar = true
	}

	return m
}

// classifyDoubleStar detects common double-star shapes so the matcher
// can dispatch without calling path.Match on every path. Unrecognized
// patterns stay as dsShapeGeneric and use the shared doubleStarMatch
// fallback. Each specialization must match the pinned single-`**`
// semantics that the behavior corpus locks in.
func classifyDoubleStar(r *trieRec) {
	// dsShapePrefixOnly: pattern ended with "**" — suffix is empty.
	// Example: "build/**". Match == filePath has prefix + "/" or equals.
	if r.dsSuffix == "" {
		r.dsShape = dsShapePrefixOnly
		return
	}
	// dsShapeMidLiteral: prefix empty and suffix is "LITERAL/**".
	// Example: "**/node_modules/**". The single-`**` quirk means
	// suffix's trailing "**" acts as a single segment matcher, which
	// reduces the pattern to "segment LITERAL appears with exactly
	// one segment after it" — i.e. LITERAL is the second-to-last
	// segment of relPath.
	if r.dsPrefix == "" && strings.HasSuffix(r.dsSuffix, "/**") {
		lit := r.dsSuffix[:len(r.dsSuffix)-len("/**")]
		if lit != "" && !strings.ContainsAny(lit, "/*?[") {
			r.dsShape = dsShapeMidLiteral
			r.dsBasenameF = lit
			return
		}
	}
	// dsShapeBasename: suffix has no "/" and no further "**".
	// Examples: "**/*.go", "src/**/Makefile", "dist/**/*.pyc".
	// Semantics reduce to: path starts with prefix (if any) AND basename
	// matches suffix as a simple glob. Classify suffix once up front.
	if !strings.Contains(r.dsSuffix, "/") && !strings.Contains(r.dsSuffix, "**") {
		k, fix := classifyGlob(r.dsSuffix)
		r.dsShape = dsShapeBasename
		r.dsBasenameK = k
		r.dsBasenameF = fix
		return
	}
	// Anything else (e.g. "a/**/b/**/c", "**/X/Y") keeps the generic
	// doubleStarMatch fallback.
	r.dsShape = dsShapeGeneric
}

// classifySegments pre-computes per-segment matchers for a glob-anchored
// pattern. One entry per segment in order.
func classifySegments(pattern string) []segGlob {
	parts := strings.Split(pattern, "/")
	out := make([]segGlob, len(parts))
	for i, seg := range parts {
		k, fix := classifyGlob(seg)
		out[i] = segGlob{raw: seg, kind: k, fixed: fix}
	}
	return out
}

// insertSegTrie walks (or creates) trie nodes for r.segGlobs and attaches
// the record at the terminal. Literal segments take the fast `literal`
// path; segments containing any wildcard use a globs edge so descent can
// still prune by segment count.
func insertSegTrie(root *segNode, r trieRec) {
	node := root
	for i, sg := range r.segGlobs {
		last := i == len(r.segGlobs)-1
		if sg.kind == kindLiteral {
			child, ok := node.literal[sg.raw]
			if !ok {
				child = &segNode{literal: map[string]*segNode{}}
				node.literal[sg.raw] = child
			}
			node = child
		} else {
			// Find an existing edge for this exact segment shape (rare
			// merge opportunity) or add a new one.
			var child *segNode
			for _, e := range node.globs {
				if e.seg.raw == sg.raw {
					child = e.child
					break
				}
			}
			if child == nil {
				child = &segNode{literal: map[string]*segNode{}}
				node.globs = append(node.globs, &segEdge{seg: sg, child: child})
			}
			node = child
		}
		if last {
			node.terminals = append(node.terminals, r)
		}
	}
}

// shouldIgnore mirrors ignoreMatcher.shouldIgnore. Builtins fire first with
// a true short-circuit. Otherwise we compute the best match per group and
// let the group-priority order (doubleStar > anchored > base) pick the
// decision's negation bit.
func (m *trieIgnoreMatcher) shouldIgnore(relPath string, isDir bool) bool {
	base := pathBaseFast(relPath)

	for i := range m.builtinBase {
		if fastMatchBaseRec(&m.builtinBase[i], base) {
			return true
		}
	}

	bestNeg := false
	hit := false

	// --- Base group --------------------------------------------------------
	baseBestOrd := -1
	baseBestNeg := false
	if m.hasLitBase {
		if recs, ok := m.litBase[base]; ok {
			for i := range recs {
				r := &recs[i]
				if r.dirOnly && !isDir {
					continue
				}
				if r.ordinal > baseBestOrd {
					baseBestOrd = r.ordinal
					baseBestNeg = r.negation
				}
			}
		}
	}
	if m.hasSuffixBuck {
		// Extension-bucketed lookup: extract the portion after the last
		// dot once, then visit only the matching bucket. For basenames
		// without a dot, the bucketed lookup is skipped entirely.
		if dot := lastDot(base); dot >= 0 {
			ext := base[dot+1:]
			if recs, ok := m.extSuffix[ext]; ok {
				for i := range recs {
					r := &recs[i]
					if r.dirOnly && !isDir {
						continue
					}
					if r.ordinal > baseBestOrd {
						baseBestOrd = r.ordinal
						baseBestNeg = r.negation
					}
				}
			}
		}
		// otherSuffix (rare: "*~" and similar) is still linear but
		// typically empty, so the range loop is a no-op branch.
		for i := range m.otherSuffix {
			r := &m.otherSuffix[i]
			if r.dirOnly && !isDir {
				continue
			}
			if strings.HasSuffix(base, r.fixed) {
				if r.ordinal > baseBestOrd {
					baseBestOrd = r.ordinal
					baseBestNeg = r.negation
				}
			}
		}
	}
	for i := range m.prefixStar {
		r := &m.prefixStar[i]
		if r.dirOnly && !isDir {
			continue
		}
		if strings.HasPrefix(base, r.fixed) {
			if r.ordinal > baseBestOrd {
				baseBestOrd = r.ordinal
				baseBestNeg = r.negation
			}
		}
	}
	for i := range m.genericBase {
		r := &m.genericBase[i]
		if r.dirOnly && !isDir {
			continue
		}
		matched, _ := path.Match(r.pattern, base)
		if matched {
			if r.ordinal > baseBestOrd {
				baseBestOrd = r.ordinal
				baseBestNeg = r.negation
			}
		}
	}
	if baseBestOrd >= 0 {
		hit = true
		bestNeg = baseBestNeg
	}

	// --- Anchored group ----------------------------------------------------
	anchBestOrd := -1
	anchBestNeg := false
	if m.hasLitAnchored {
		if recs, ok := m.litAnchored[relPath]; ok {
			for i := range recs {
				r := &recs[i]
				if r.dirOnly && !isDir {
					continue
				}
				if r.ordinal > anchBestOrd {
					anchBestOrd = r.ordinal
					anchBestNeg = r.negation
				}
			}
		}
	}
	if m.hasAnchoredGlob {
		segs := splitSegments(relPath)
		walkAnchoredTrie(m.anchoredRoot, segs, 0, isDir, &anchBestOrd, &anchBestNeg)
	}
	if anchBestOrd >= 0 {
		hit = true
		bestNeg = anchBestNeg
	}

	// --- Double-star group -------------------------------------------------
	if m.hasDoubleStar {
		dsBestOrd := -1
		dsBestNeg := false
		// secondToLast is computed at most once per call, only if any
		// dsShapeMidLiteral record needs it.
		secondToLastSeg := ""
		secondToLastKnown := false
		for i := range m.doubleStar {
			r := &m.doubleStar[i]
			if r.dirOnly && !isDir {
				continue
			}
			var matched bool
			switch r.dsShape {
			case dsShapePrefixOnly:
				// "prefix/**" matches filePath starting with "prefix/" or
				// equalling "prefix". Empty prefix matches anything.
				if r.dsPrefix == "" {
					matched = true
				} else {
					matched = relPath == r.dsPrefix ||
						(len(relPath) > len(r.dsPrefix) &&
							relPath[len(r.dsPrefix)] == '/' &&
							relPath[:len(r.dsPrefix)] == r.dsPrefix)
				}
			case dsShapeMidLiteral:
				if !secondToLastKnown {
					secondToLastSeg = secondToLastSegment(relPath)
					secondToLastKnown = true
				}
				matched = secondToLastSeg != "" && secondToLastSeg == r.dsBasenameF
			case dsShapeBasename:
				if r.dsPrefix != "" {
					ok := relPath == r.dsPrefix ||
						(len(relPath) > len(r.dsPrefix) &&
							relPath[len(r.dsPrefix)] == '/' &&
							relPath[:len(r.dsPrefix)] == r.dsPrefix)
					if !ok {
						break
					}
				}
				matched = matchBasenameKind(r.dsBasenameK, r.dsBasenameF, r.dsSuffix, base)
			default:
				matched = doubleStarMatch(r.dsPrefix, r.dsSuffix, relPath)
			}
			if matched {
				if r.ordinal > dsBestOrd {
					dsBestOrd = r.ordinal
					dsBestNeg = r.negation
				}
			}
		}
		if dsBestOrd >= 0 {
			hit = true
			bestNeg = dsBestNeg
		}
	}

	if !hit {
		return false
	}
	return !bestNeg
}

// secondToLastSegment returns the segment just before the last "/" in p,
// or "" when p has fewer than two segments. Zero-allocation; scans p
// at most twice from the end.
func secondToLastSegment(p string) string {
	last := strings.LastIndexByte(p, '/')
	if last <= 0 {
		return ""
	}
	prev := strings.LastIndexByte(p[:last], '/')
	return p[prev+1 : last]
}

// matchBasenameKind dispatches a pre-classified basename pattern against
// a basename. Mirrors fastMatchBaseRec but avoids carrying a full
// trieRec: dsShapeBasename only needs kind, fixed, and the raw pattern
// for the kindGeneric fallback.
func matchBasenameKind(kind patternKind, fixed, raw, base string) bool {
	switch kind {
	case kindLiteral:
		return base == fixed
	case kindStarSuffix:
		return strings.HasSuffix(base, fixed)
	case kindPrefixStar:
		return strings.HasPrefix(base, fixed)
	case kindContains:
		return strings.Contains(base, fixed)
	default:
		matched, _ := path.Match(raw, base)
		return matched
	}
}

// pathBaseFast is a lighter alternative to path.Base for the subset of
// inputs this matcher sees: non-empty relative paths with forward
// slashes, no cleanup needed. path.Base does extra work for trailing
// slashes and empty inputs that cannot occur here.
func pathBaseFast(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// lastDot returns the index of the last "." in base, or -1 if none.
// Inlined so the hot path avoids a function call on every match.
func lastDot(base string) int {
	return strings.LastIndexByte(base, '.')
}

// splitSegments returns the path's /-separated segments. Empty relPath
// yields an empty slice so the trie walk terminates immediately.
func splitSegments(relPath string) []string {
	if relPath == "" {
		return nil
	}
	return strings.Split(relPath, "/")
}

// walkAnchoredTrie descends the trie one segment at a time, recording
// the best terminal match. Literal children are taken first; glob edges
// are checked via per-segment path.Match.
func walkAnchoredTrie(node *segNode, segs []string, depth int, isDir bool, bestOrd *int, bestNeg *bool) {
	if depth == len(segs) {
		for i := range node.terminals {
			r := &node.terminals[i]
			if r.dirOnly && !isDir {
				continue
			}
			if r.ordinal > *bestOrd {
				*bestOrd = r.ordinal
				*bestNeg = r.negation
			}
		}
		return
	}
	seg := segs[depth]
	if child, ok := node.literal[seg]; ok {
		walkAnchoredTrie(child, segs, depth+1, isDir, bestOrd, bestNeg)
	}
	for _, e := range node.globs {
		if matchSegment(&e.seg, seg) {
			walkAnchoredTrie(e.child, segs, depth+1, isDir, bestOrd, bestNeg)
		}
	}
}

// matchSegment tests a single pre-classified glob segment against a single
// path segment. Fast paths mirror fastMatchBase; generic falls back to
// path.Match of the whole segment.
func matchSegment(sg *segGlob, s string) bool {
	switch sg.kind {
	case kindLiteral:
		return s == sg.fixed
	case kindStarSuffix:
		return strings.HasSuffix(s, sg.fixed)
	case kindPrefixStar:
		return strings.HasPrefix(s, sg.fixed)
	case kindContains:
		return strings.Contains(s, sg.fixed)
	default:
		matched, _ := path.Match(sg.raw, s)
		return matched
	}
}

// fastMatchBaseRec is the trieRec flavor of fastMatchBase; identical
// fast-paths, different record type.
func fastMatchBaseRec(r *trieRec, base string) bool {
	switch r.kind {
	case kindLiteral:
		return base == r.fixed
	case kindStarSuffix:
		return strings.HasSuffix(base, r.fixed)
	case kindPrefixStar:
		return strings.HasPrefix(base, r.fixed)
	case kindContains:
		return strings.Contains(base, r.fixed)
	default:
		matched, _ := path.Match(r.pattern, base)
		return matched
	}
}
