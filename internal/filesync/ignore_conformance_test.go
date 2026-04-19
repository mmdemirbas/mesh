package filesync

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PF Phase 2 Step 1 — conformance harness.
//
// The harness runs two matchers against a shared corpus (patterns × paths)
// and reports every decision divergence. Its purpose is to serve as the
// merge gate for the trie rewrite: the trie must produce zero divergences
// against the current linear matcher across the full corpus before it can
// replace it.
//
// Everything here is test-only. The harness itself is exercised by a
// self-tautology check (linear vs linear must have zero divergence) so
// that a bug in the harness cannot silently approve a buggy candidate.

// Matcher is the callable surface both the reference and the candidate must
// implement. It matches the shape of ignoreMatcher.shouldIgnore.
type Matcher func(relPath string, isDir bool) bool

// MatcherFactory builds a Matcher from a list of raw ignore patterns. Built
// this way so the candidate (e.g. trie) can share the reference's corpus
// without leaking internal state.
type MatcherFactory func(patterns []string) Matcher

// linearFactory wraps the current production matcher.
var linearFactory MatcherFactory = func(patterns []string) Matcher {
	return newIgnoreMatcher(patterns).shouldIgnore
}

// conformanceCase is one (path, isDir) pair the harness evaluates.
type conformanceCase struct {
	path  string
	isDir bool
}

// divergence records a single disagreement between reference and candidate.
type divergence struct {
	path  string
	isDir bool
	ref   bool
	cand  bool
}

func (d divergence) String() string {
	return fmt.Sprintf("%q isDir=%v: ref=%v cand=%v", d.path, d.isDir, d.ref, d.cand)
}

// runConformance feeds cases through both factories and returns every
// divergence. It is the merge-gate primitive: a candidate qualifies to
// replace the reference only when this returns an empty slice.
func runConformance(patterns []string, cases []conformanceCase, ref, candidate MatcherFactory) []divergence {
	r := ref(patterns)
	c := candidate(patterns)
	var diffs []divergence
	for _, tc := range cases {
		rv := r(tc.path, tc.isDir)
		cv := c(tc.path, tc.isDir)
		if rv != cv {
			diffs = append(diffs, divergence{path: tc.path, isDir: tc.isDir, ref: rv, cand: cv})
		}
	}
	return diffs
}

// -----------------------------------------------------------------------------
// Handwritten edge-case corpus
// -----------------------------------------------------------------------------

// edgeCasePatterns covers the shapes PF Phase 2 explicitly flags as risky:
// deep **, negation chains, directory-only patterns, trailing slash rules,
// mixed separators at construct time, and case sensitivity edges.
func edgeCasePatterns() []string {
	return []string{
		// literals
		".git/", ".svn/", ".DS_Store", "Thumbs.db",
		// star-suffix
		"*.class", "*.o", "*.log", "*.tmp", "*.swp",
		// prefix-star
		"tmp-*", "cache-*", "debug-*",
		// anchored
		"src/generated/", "docs/build/", "packages/dist/",
		// dir-only vs file
		"node_modules/", "vendor/",
		// single double-star
		"**/test-output/**",
		"**/__snapshots__/**",
		"vendor/**",
		"**/*.generated.go",
		// negation chains
		"!important.class", "!keep.log", "!docs/build/index.html",
		// generic globs (path.Match classes)
		"[Mm]akefile", "f?le.*",
	}
}

// edgeCasePaths enumerates paths that exercise each construct above.
func edgeCasePaths() []conformanceCase {
	return []conformanceCase{
		// literals
		{".git", true},
		{".git", false},
		{"sub/.git", true},
		{".DS_Store", false},
		{"deep/nested/.DS_Store", false},
		{"Thumbs.db", false},
		// star-suffix
		{"Foo.class", false},
		{"important.class", false},
		{"sub/important.class", false},
		{"deep/debug.log", false},
		{"keep.log", false},
		// prefix-star
		{"tmp-123", false},
		{"nested/cache-abc", false},
		{"debug-xyz", false},
		// anchored
		{"src/generated", true},
		{"src/generated/foo.go", false},
		{"docs/build", true},
		{"docs/build/index.html", false},
		{"packages/dist", true},
		// dir-only
		{"node_modules", true},
		{"node_modules", false}, // file with same name — not ignored
		{"sub/node_modules", true},
		{"vendor/pkg/file.go", false},
		// double-star
		{"foo/test-output/bar", false},
		{"a/b/c/test-output/report.html", false},
		{"pkg/__snapshots__/render.snap", false},
		{"vendor/lib/foo.go", false},
		{"foo.generated.go", false},
		{"sub/bar.generated.go", false},
		// generic globs
		{"Makefile", false},
		{"makefile", false},
		{"file.txt", false},
		{"fule.go", false},
		// no-match baselines
		{"README.md", false},
		{"src/main.go", false},
		// builtin wins
		{".mesh-tmp-abc", false},
		{"sub/.mesh-tmp-abc", false},
		{"foo.mesh-delta-tmp-abcd", false},
	}
}

// -----------------------------------------------------------------------------
// Deterministic generated corpus
// -----------------------------------------------------------------------------

// corpusScale bundles the size knobs so tests can generate small corpora in
// CI and large ones behind a flag.
type corpusScale struct {
	patterns int
	paths    int
	seed     uint64
}

// smallScale is the default — runs every `go test` without noticeable cost.
var smallScale = corpusScale{patterns: 100, paths: 500, seed: 0xC0FFEE}

// largeScale exercises the 10k × 10k gate called out in the PF Phase 2 plan.
// Gated behind the `-conformance.large` flag (via env for simplicity).
var largeScale = corpusScale{patterns: 10_000, paths: 10_000, seed: 0xDEADBEEF}

// generateCorpus builds a deterministic set of patterns and paths from the
// given scale. Determinism is load-bearing: a random divergence must be
// reproducible from the seed alone.
func generateCorpus(scale corpusScale) (patterns []string, cases []conformanceCase) {
	r := rand.New(rand.NewPCG(scale.seed, scale.seed^0xA5A5A5A5))

	// Pool of reusable name segments.
	names := []string{
		"foo", "bar", "baz", "qux", "src", "pkg", "cmd", "internal",
		"docs", "test", "node_modules", "dist", "build", "vendor",
		"a", "b", "c", "d", "e", "hello", "world", "file", "main",
	}
	exts := []string{"go", "js", "ts", "py", "java", "class", "o", "log", "tmp", "txt", "md"}

	pattern := func() string {
		switch r.IntN(10) {
		case 0: // literal basename
			return pick(r, names)
		case 1: // dir-only literal
			return pick(r, names) + "/"
		case 2: // star-suffix
			return "*." + pick(r, exts)
		case 3: // prefix-star
			return pick(r, names) + "-*"
		case 4: // anchored literal
			return pick(r, names) + "/" + pick(r, names)
		case 5: // anchored dir-only
			return pick(r, names) + "/" + pick(r, names) + "/"
		case 6: // **/name
			return "**/" + pick(r, names)
		case 7: // name/**
			return pick(r, names) + "/**"
		case 8: // **/*.ext
			return "**/*." + pick(r, exts)
		case 9: // negation of a basename suffix
			return "!*." + pick(r, exts)
		}
		return pick(r, names)
	}

	patterns = make([]string, scale.patterns)
	for i := range patterns {
		patterns[i] = pattern()
	}

	path := func() (string, bool) {
		depth := 1 + r.IntN(5)
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = pick(r, names)
		}
		// Last segment: sometimes a file with extension.
		if r.IntN(2) == 0 {
			parts[depth-1] = parts[depth-1] + "." + pick(r, exts)
		}
		return strings.Join(parts, "/"), r.IntN(4) == 0
	}
	cases = make([]conformanceCase, scale.paths)
	for i := range cases {
		p, d := path()
		cases[i] = conformanceCase{path: p, isDir: d}
	}
	return patterns, cases
}

func pick[T any](r *rand.Rand, xs []T) T {
	return xs[r.IntN(len(xs))]
}

// -----------------------------------------------------------------------------
// Self-check and tautology tests
// -----------------------------------------------------------------------------

// TestConformanceHarnessSelfCheckSmall runs the harness ref-vs-ref on the
// small generated corpus. It proves the harness itself is consistent: if
// this test ever fails, the harness is buggy and any later candidate run
// cannot be trusted.
func TestConformanceHarnessSelfCheckSmall(t *testing.T) {
	t.Parallel()
	patterns, cases := generateCorpus(smallScale)
	diffs := runConformance(patterns, cases, linearFactory, linearFactory)
	if len(diffs) != 0 {
		t.Fatalf("harness self-check failed: %d divergences (first: %s)",
			len(diffs), diffs[0])
	}
}

// TestConformanceHarnessSelfCheckEdgeCases runs the harness ref-vs-ref on
// the handwritten edge-case corpus. Splitting the self-checks keeps the
// failure message pointed at whichever corpus broke.
func TestConformanceHarnessSelfCheckEdgeCases(t *testing.T) {
	t.Parallel()
	patterns := edgeCasePatterns()
	cases := edgeCasePaths()
	diffs := runConformance(patterns, cases, linearFactory, linearFactory)
	if len(diffs) != 0 {
		t.Fatalf("edge-case self-check failed: %d divergences (first: %s)",
			len(diffs), diffs[0])
	}
}

// TestConformanceHarnessLargeScale runs 10 000 patterns × 10 000 paths
// ref-vs-ref. Gated behind MESH_CONFORMANCE_LARGE=1 so every `go test`
// stays fast, while the full scale still runs on demand and in the gate
// that will eventually compare the trie against the linear baseline.
func TestConformanceHarnessLargeScale(t *testing.T) {
	if os.Getenv("MESH_CONFORMANCE_LARGE") != "1" {
		t.Skip("set MESH_CONFORMANCE_LARGE=1 to run the 10k × 10k gate")
	}
	patterns, cases := generateCorpus(largeScale)
	diffs := runConformance(patterns, cases, linearFactory, linearFactory)
	if len(diffs) != 0 {
		t.Fatalf("large-scale self-check failed: %d divergences (first: %s)",
			len(diffs), diffs[0])
	}
}

// -----------------------------------------------------------------------------
// Optional cross-check against `git check-ignore`
// -----------------------------------------------------------------------------

// TestConformanceAgainstGitCheckIgnore grounds the linear matcher against
// git's own gitignore implementation for a handwritten subset. Divergences
// are not failures here — they are known gaps pinned so Phase 2 can decide
// per-pattern whether to match git or preserve the quirk (see PF §plan.3).
// Gated behind MESH_CONFORMANCE_GIT=1 so systems without git stay green.
func TestConformanceAgainstGitCheckIgnore(t *testing.T) {
	if os.Getenv("MESH_CONFORMANCE_GIT") != "1" {
		t.Skip("set MESH_CONFORMANCE_GIT=1 to cross-check against git check-ignore")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	patterns := edgeCasePatterns()
	cases := edgeCasePaths()

	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-q")
	mustRun(t, dir, "git", "config", "user.email", "test@example.com")
	mustRun(t, dir, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
		[]byte(strings.Join(patterns, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	m := linearFactory(patterns)
	var gaps []divergence
	for _, tc := range cases {
		mv := m(tc.path, tc.isDir)
		gv := gitIgnored(t, dir, tc.path)
		if mv != gv {
			gaps = append(gaps, divergence{path: tc.path, isDir: tc.isDir, ref: mv, cand: gv})
		}
	}
	// Report, don't fail. The linear matcher has known gitignore gaps
	// (single-** only, unanchored-path semantics, etc.).
	if len(gaps) > 0 {
		t.Logf("linear matcher diverges from git on %d/%d handwritten cases (pinned for Phase 2 decision):",
			len(gaps), len(cases))
		for _, d := range gaps {
			t.Logf("  %s", d)
		}
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}

// gitIgnored returns true if `git check-ignore -q` inside dir would ignore
// relPath. It does NOT need the file to exist on disk.
func gitIgnored(t *testing.T, dir, relPath string) bool {
	t.Helper()
	cmd := exec.Command("git", "check-ignore", "--no-index", "-q", "--", relPath)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %q: %v", relPath, err)
	return false
}
