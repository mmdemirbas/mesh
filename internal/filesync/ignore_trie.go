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
//   - Builtins: compiled into an inlined HasPrefix/Contains check on the
//     basename. No loop, no switch, no per-entry record.
//   - Base (pattern has neither "/" nor "**"): literals keyed by
//     basename, *.ext patterns keyed by extension, remaining
//     prefix-star / generic / non-extension-suffix patterns in slices
//     iterated in reverse (last registration wins, break on match).
//   - Anchored (has "/", no "**"): literal full-path → map lookup;
//     glob-anchored → per-segment trie.
//   - Double-star (has "**"): pre-classified into dsShape fast paths;
//     generic falls back to doubleStarMatch.
//
// Cross-group priority (base < anchored < doubleStar) is applied by the
// sequential `bestNeg = ...` writes in shouldIgnore. Within each group,
// map-based lookups are pre-collapsed to last-wins at construction and
// slice-based groups are iterated backward so the first match is the
// highest-ordinal one.
type trieIgnoreMatcher struct {
	// Base group.
	litBase     map[string]baseFlags // keyed by basename
	extSuffix   map[string]baseFlags // keyed by extension (after last ".")
	prefixStar  []baseTest           // fixed = prefix literal
	genericBase []baseTest           // fixed = full pattern (path.Match fallback)
	otherSuffix []baseTest           // non-extension kindStarSuffix (e.g. "*~")

	// Anchored group.
	litAnchored  map[string]baseFlags // keyed by full relPath
	anchoredRoot *segNode             // nil if no glob-anchored patterns

	// Double-star group. Kept as trieRec because the match-time dispatch
	// needs several pre-classified fields.
	doubleStar []trieRec

	// Skip flags: shouldIgnore reads these before touching the group so
	// empty groups cost only a predictable branch. Adds up across 310k
	// files per scan.
	hasLitBase      bool
	hasSuffixBuck   bool
	hasPrefixStar   bool
	hasGeneric      bool
	hasOtherSuffix  bool
	hasLitAnchored  bool
	hasAnchoredGlob bool
	hasDoubleStar   bool

	// First-char bitmaps for the two base-group maps. At construction
	// each set bit `1 << (firstByte & 63)` indicates that at least one
	// registered key has that first-byte class. A ~2ns bitmap test
	// rejects the majority of misses so we don't pay ~8ns per map lookup
	// on every file. Collisions on `&63` are tolerated — a false positive
	// falls back to the map lookup, which is the same cost we'd pay
	// without the filter.
	litBaseFirstByte   uint64
	extSuffixFirstByte uint64
	// prefixStarFirstByte covers the prefix-star slice: each pattern
	// requires base to start with its `fixed` string, so base[0] must
	// match at least one registered `fixed[0]`.
	prefixStarFirstByte uint64
	// otherSuffixLastByte covers kindStarSuffix patterns that don't live
	// in the extSuffix map ("*~", "*bak"). base must END with the fixed
	// suffix, so base's last byte must match at least one registered
	// fixed-suffix last byte.
	otherSuffixLastByte uint64
	// litAnchoredFirstByte covers the same purpose for the litAnchored
	// map keyed by full relPath.
	litAnchoredFirstByte uint64
}

// baseFlags is the slim payload stored in the litBase / extSuffix /
// litAnchored maps. The key carries the fixed portion; this struct
// carries only the bits the match step consults. 8 bytes with padding.
type baseFlags struct {
	ordinal  uint32
	negation bool
	dirOnly  bool
	// present is written by the constructor's last-wins merge so the
	// zero value is distinguishable from an explicit entry. Without it,
	// map-not-present and map-present-with-defaults would be ambiguous
	// in future refactors; the field costs nothing in practice.
	present bool
}

// baseTest carries the fixed portion inline — used by slice-based groups
// (prefixStar / otherSuffix / genericBase). 32 bytes with padding.
type baseTest struct {
	fixed    string // prefix / suffix / full pattern
	ordinal  uint32
	negation bool
	dirOnly  bool
}

// trieRec is retained for the double-star group and for the anchored
// segment trie. Base and anchored-literal groups use the slim baseFlags
// and baseTest structs instead.
type trieRec struct {
	pattern  string
	negation bool
	dirOnly  bool
	kind     patternKind
	fixed    string
	dsPrefix string
	dsSuffix string
	ordinal  uint32
	// segGlobs holds per-segment globs for an anchored pattern that lives
	// in anchoredRoot. Empty for doublestar records.
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

// builtinIgnores is a compile-time constant: the two patterns are baked
// directly into shouldIgnore as an inlined HasPrefix / Contains check.
// The strings below mirror the package-level builtinIgnores list in
// ignore.go; keep them in sync. A guard test in ignore_behavior_test.go
// verifies the hardcoded forms still cover those patterns.
const (
	builtinTmpPrefix     = ".mesh-tmp-"        // from ".mesh-tmp-*"
	builtinDeltaContains = ".mesh-delta-tmp-" // from "*.mesh-delta-tmp-*"
)

// newTrieIgnoreMatcher builds the parallel matcher. Patterns are parsed
// identically to newIgnoreMatcher so group membership is identical; only
// the per-group storage differs.
func newTrieIgnoreMatcher(configPatterns []string) *trieIgnoreMatcher {
	m := &trieIgnoreMatcher{
		litBase:     map[string]baseFlags{},
		extSuffix:   map[string]baseFlags{},
		litAnchored: map[string]baseFlags{},
	}

	// Per-group ordinals: incremented within each group so we can pick
	// the highest-ordinal match per group at decision time.
	var baseOrd, anchoredOrd, doubleOrd uint32

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
			kind, _ := classifyGlob(p.pattern)
			flags := baseFlags{
				ordinal:  anchoredOrd,
				negation: p.negation,
				dirOnly:  p.dirOnly,
				present:  true,
			}
			if kind == kindLiteral {
				mergeBaseFlags(m.litAnchored, p.pattern, flags)
				m.hasLitAnchored = true
				if len(p.pattern) > 0 {
					m.litAnchoredFirstByte |= 1 << (p.pattern[0] & 63)
				}
			} else {
				r := trieRec{
					pattern:  p.pattern,
					negation: p.negation,
					dirOnly:  p.dirOnly,
					ordinal:  anchoredOrd,
					kind:     kind,
				}
				r.segGlobs = classifySegments(p.pattern)
				if m.anchoredRoot == nil {
					m.anchoredRoot = &segNode{literal: map[string]*segNode{}}
				}
				insertSegTrie(m.anchoredRoot, r)
				m.hasAnchoredGlob = true
			}
			anchoredOrd++
		default:
			kind, fixed := classifyGlob(p.pattern)
			flags := baseFlags{
				ordinal:  baseOrd,
				negation: p.negation,
				dirOnly:  p.dirOnly,
				present:  true,
			}
			switch kind {
			case kindLiteral:
				mergeBaseFlags(m.litBase, p.pattern, flags)
				m.hasLitBase = true
				if len(p.pattern) > 0 {
					m.litBaseFirstByte |= 1 << (p.pattern[0] & 63)
				}
			case kindStarSuffix:
				// "*.ext" patterns (fixed=".ext") are bucketed by the
				// extension so a basename ending in ".ext" can find
				// its candidates via one map lookup. Non-extension
				// suffixes (e.g. "*~" → fixed="~") stay in otherSuffix.
				if len(fixed) >= 2 && fixed[0] == '.' && !strings.ContainsAny(fixed[1:], ".") {
					ext := fixed[1:]
					mergeBaseFlags(m.extSuffix, ext, flags)
					m.hasSuffixBuck = true
					if len(ext) > 0 {
						m.extSuffixFirstByte |= 1 << (ext[0] & 63)
					}
				} else {
					m.otherSuffix = append(m.otherSuffix, baseTest{
						fixed:    fixed,
						ordinal:  baseOrd,
						negation: p.negation,
						dirOnly:  p.dirOnly,
					})
					m.hasOtherSuffix = true
					if n := len(fixed); n > 0 {
						m.otherSuffixLastByte |= 1 << (fixed[n-1] & 63)
					}
				}
			case kindPrefixStar:
				m.prefixStar = append(m.prefixStar, baseTest{
					fixed:    fixed,
					ordinal:  baseOrd,
					negation: p.negation,
					dirOnly:  p.dirOnly,
				})
				m.hasPrefixStar = true
				if len(fixed) > 0 {
					m.prefixStarFirstByte |= 1 << (fixed[0] & 63)
				}
			default:
				m.genericBase = append(m.genericBase, baseTest{
					fixed:    p.pattern,
					ordinal:  baseOrd,
					negation: p.negation,
					dirOnly:  p.dirOnly,
				})
				m.hasGeneric = true
			}
			baseOrd++
		}
	}

	m.hasDoubleStar = len(m.doubleStar) > 0
	return m
}

// mergeBaseFlags applies last-wins for map buckets: if a pattern with
// key `k` already exists, the higher ordinal (always the new one, since
// we insert in order) replaces it. This collapses `map[string][]rec`
// into `map[string]rec`, saving a slice header + heap allocation per
// bucket and a per-match range loop.
func mergeBaseFlags(dst map[string]baseFlags, k string, v baseFlags) {
	// Insertion is strictly in ordinal order so we can unconditionally
	// overwrite: any existing entry has a lower ordinal.
	dst[k] = v
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

// shouldIgnore mirrors ignoreMatcher.shouldIgnore. Builtins fire first
// with a true short-circuit via inlined HasPrefix/Contains checks; no
// record loop is executed. The three configurable groups are evaluated
// in base → anchored → doubleStar order with last-match-wins semantics
// within each group.
func (m *trieIgnoreMatcher) shouldIgnore(relPath string, isDir bool) bool {
	// One slash scan: base extraction, anchored-group guard, and
	// secondToLastSegment all reuse lastSlash. Profiling showed the
	// split scans were the biggest remaining waste on realistic paths.
	lastSlash := strings.LastIndexByte(relPath, '/')
	var base string
	if lastSlash >= 0 {
		base = relPath[lastSlash+1:]
	} else {
		base = relPath
	}

	// Built-ins are non-negatable and compile-time known, so we skip the
	// generic pattern machinery entirely. Kept in sync with
	// builtinIgnores in ignore.go; see the guard test.
	if strings.HasPrefix(base, builtinTmpPrefix) ||
		strings.Contains(base, builtinDeltaContains) {
		return true
	}

	bestOrd := int32(-1)
	bestNeg := false

	// --- Base group --------------------------------------------------------
	// Map lookups are flattened to single baseFlags values (last-wins at
	// construction), so the hot path is a single map read plus two
	// branches. Slice-based sub-groups iterate backward and break on
	// first match, so the highest-ordinal hit is found with minimum
	// work.
	baseBest := int32(-1)
	baseNeg := false
	// First-byte bitmap pre-filter: if no registered key shares base[0]'s
	// 64-way class, the map lookup cannot hit. ~2ns test, ~8ns saved per
	// skip. base is non-empty because lastSlash guarantees a remaining
	// tail and relPath was non-empty.
	fb := uint64(1) << (base[0] & 63)
	if m.hasLitBase && m.litBaseFirstByte&fb != 0 {
		if f, ok := m.litBase[base]; ok && (!f.dirOnly || isDir) {
			if int32(f.ordinal) > baseBest {
				baseBest = int32(f.ordinal)
				baseNeg = f.negation
			}
		}
	}
	if m.hasSuffixBuck {
		if dot := strings.LastIndexByte(base, '.'); dot >= 0 && dot < len(base)-1 {
			ext := base[dot+1:]
			if m.extSuffixFirstByte&(uint64(1)<<(ext[0]&63)) != 0 {
				if f, ok := m.extSuffix[ext]; ok && (!f.dirOnly || isDir) {
					if int32(f.ordinal) > baseBest {
						baseBest = int32(f.ordinal)
						baseNeg = f.negation
					}
				}
			}
		}
	}
	if m.hasOtherSuffix {
		// base's last byte must match at least one registered suffix's
		// last byte, otherwise no HasSuffix can succeed. Cheap reject.
		if m.otherSuffixLastByte&(uint64(1)<<(base[len(base)-1]&63)) != 0 {
			for i := len(m.otherSuffix) - 1; i >= 0; i-- {
				t := &m.otherSuffix[i]
				if t.dirOnly && !isDir {
					continue
				}
				if int32(t.ordinal) <= baseBest {
					// Remaining entries have lower ordinals; stop.
					break
				}
				if strings.HasSuffix(base, t.fixed) {
					baseBest = int32(t.ordinal)
					baseNeg = t.negation
					break
				}
			}
		}
	}
	if m.hasPrefixStar && m.prefixStarFirstByte&fb != 0 {
		for i := len(m.prefixStar) - 1; i >= 0; i-- {
			t := &m.prefixStar[i]
			if t.dirOnly && !isDir {
				continue
			}
			if int32(t.ordinal) <= baseBest {
				break
			}
			if strings.HasPrefix(base, t.fixed) {
				baseBest = int32(t.ordinal)
				baseNeg = t.negation
				break
			}
		}
	}
	if m.hasGeneric {
		for i := len(m.genericBase) - 1; i >= 0; i-- {
			t := &m.genericBase[i]
			if t.dirOnly && !isDir {
				continue
			}
			if int32(t.ordinal) <= baseBest {
				break
			}
			if matched, _ := path.Match(t.fixed, base); matched {
				baseBest = int32(t.ordinal)
				baseNeg = t.negation
				break
			}
		}
	}
	if baseBest >= 0 {
		bestOrd = baseBest
		bestNeg = baseNeg
	}

	// --- Anchored group ----------------------------------------------------
	// Anchored patterns always contain "/"; on paths without one, skip
	// both sub-groups outright. Reuses lastSlash from the top of the
	// function to avoid a second scan.
	if (m.hasLitAnchored || m.hasAnchoredGlob) && lastSlash >= 0 {
		anchBest := int32(-1)
		anchNeg := false
		if m.hasLitAnchored && m.litAnchoredFirstByte&(uint64(1)<<(relPath[0]&63)) != 0 {
			if f, ok := m.litAnchored[relPath]; ok && (!f.dirOnly || isDir) {
				if int32(f.ordinal) > anchBest {
					anchBest = int32(f.ordinal)
					anchNeg = f.negation
				}
			}
		}
		if m.hasAnchoredGlob {
			segs := splitSegments(relPath)
			walkAnchoredTrie(m.anchoredRoot, segs, 0, isDir, &anchBest, &anchNeg)
		}
		if anchBest >= 0 {
			bestOrd = anchBest
			bestNeg = anchNeg
		}
	}

	// --- Double-star group -------------------------------------------------
	if m.hasDoubleStar {
		dsBest := int32(-1)
		dsNeg := false
		secondToLastSeg := ""
		secondToLastKnown := false
		for i := len(m.doubleStar) - 1; i >= 0; i-- {
			r := &m.doubleStar[i]
			if r.dirOnly && !isDir {
				continue
			}
			if int32(r.ordinal) <= dsBest {
				break
			}
			var matched bool
			switch r.dsShape {
			case dsShapePrefixOnly:
				matched = r.dsPrefix == "" ||
					relPath == r.dsPrefix ||
					(len(relPath) > len(r.dsPrefix) &&
						relPath[len(r.dsPrefix)] == '/' &&
						relPath[:len(r.dsPrefix)] == r.dsPrefix)
			case dsShapeMidLiteral:
				if !secondToLastKnown {
					// Inline lastSlash reuse: no trailing segment → empty;
					// otherwise scan only the prefix [:lastSlash] once.
					if lastSlash <= 0 {
						secondToLastSeg = ""
					} else {
						prev := strings.LastIndexByte(relPath[:lastSlash], '/')
						secondToLastSeg = relPath[prev+1 : lastSlash]
					}
					secondToLastKnown = true
				}
				matched = secondToLastSeg == r.dsBasenameF && secondToLastSeg != ""
			case dsShapeBasename:
				if r.dsPrefix != "" {
					ok := relPath == r.dsPrefix ||
						(len(relPath) > len(r.dsPrefix) &&
							relPath[len(r.dsPrefix)] == '/' &&
							relPath[:len(r.dsPrefix)] == r.dsPrefix)
					if !ok {
						continue
					}
				}
				matched = matchBasenameKind(r.dsBasenameK, r.dsBasenameF, r.dsSuffix, base)
			default:
				matched = doubleStarMatch(r.dsPrefix, r.dsSuffix, relPath)
			}
			if matched {
				dsBest = int32(r.ordinal)
				dsNeg = r.negation
				break
			}
		}
		if dsBest >= 0 {
			bestOrd = dsBest
			bestNeg = dsNeg
		}
	}

	if bestOrd < 0 {
		return false
	}
	return !bestNeg
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
func walkAnchoredTrie(node *segNode, segs []string, depth int, isDir bool, bestOrd *int32, bestNeg *bool) {
	if depth == len(segs) {
		for i := range node.terminals {
			r := &node.terminals[i]
			if r.dirOnly && !isDir {
				continue
			}
			if int32(r.ordinal) > *bestOrd {
				*bestOrd = int32(r.ordinal)
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
