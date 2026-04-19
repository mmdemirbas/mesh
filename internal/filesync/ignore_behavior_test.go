package filesync

import (
	"fmt"
	"testing"
)

// Comprehensive behavior-pinning corpus for the ignore matcher.
//
// Goal: lock the current linear matcher's decisions across every construct
// it supports. Any candidate implementation (e.g. the PF Phase 2 trie) must
// produce identical output on every case before it can replace the linear
// matcher. Cases are deliberately hand-written — the expected value encodes
// understanding of the matcher, not a circular read of its own output.
//
// Each table isolates one construct so a failure points at the exact
// shape that regressed. The runner at the bottom dispatches a factory
// through every table so both the linear and the trie implementation
// share one corpus.

type behaviorCase struct {
	patterns []string
	path     string
	isDir    bool
	want     bool
}

// --- builtin, non-negatable -------------------------------------------------

var behaviorBuiltins = []behaviorCase{
	// Builtins match at any depth via basename.
	{nil, ".mesh-tmp-abc", false, true},
	{nil, "sub/.mesh-tmp-abc", false, true},
	{nil, "a/b/c/.mesh-tmp-z", false, true},
	{nil, "foo.mesh-delta-tmp-0123", false, true},
	{nil, "sub/bar.mesh-delta-tmp-0123", false, true},
	// Builtins are non-negatable: user rules cannot un-ignore them.
	{[]string{"!.mesh-tmp-*"}, ".mesh-tmp-abc", false, true},
	{[]string{"!*.mesh-delta-tmp-*"}, "foo.mesh-delta-tmp-abcd", false, true},
	// Non-matching names stay un-ignored.
	{nil, "mesh-tmp-abc", false, false}, // missing leading dot
	{nil, ".mesh-tmp", false, false},    // trailing segment required by prefix-star
	{nil, "foo.mesh-delta-tmp", false, false},
	{nil, "README.md", false, false},
}

// --- literal basename (no slash, no wildcard) -------------------------------

var behaviorLiteralBase = []behaviorCase{
	// Root and nested.
	{[]string{".DS_Store"}, ".DS_Store", false, true},
	{[]string{".DS_Store"}, "sub/.DS_Store", false, true},
	{[]string{".DS_Store"}, "a/b/c/.DS_Store", false, true},
	{[]string{".DS_Store"}, "DS_Store", false, false},
	{[]string{".DS_Store"}, ".DS_Store.bak", false, false},
	{[]string{"Thumbs.db"}, "Thumbs.db", false, true},
	{[]string{"Thumbs.db"}, "thumbs.db", false, false}, // case-sensitive

	// Directories — literal (no trailing /) matches both files and dirs.
	{[]string{".git"}, ".git", true, true},
	{[]string{".git"}, ".git", false, true},
	{[]string{".git"}, "sub/.git", true, true},
}

// --- dir-only literal (trailing /) ------------------------------------------

var behaviorDirOnlyLiteral = []behaviorCase{
	{[]string{".git/"}, ".git", true, true},
	{[]string{".git/"}, ".git", false, false}, // dir-only: file named .git is NOT ignored
	{[]string{".git/"}, "sub/.git", true, true},
	{[]string{".git/"}, "sub/.git", false, false},
	{[]string{"build/"}, "build", true, true},
	{[]string{"build/"}, "build", false, false},
	{[]string{"node_modules/"}, "node_modules", true, true},
	{[]string{"node_modules/"}, "node_modules", false, false},
	{[]string{"node_modules/"}, "pkg/node_modules", true, true},
}

// --- star-suffix (*.ext) — basename-only ------------------------------------

var behaviorStarSuffix = []behaviorCase{
	{[]string{"*.class"}, "Foo.class", false, true},
	{[]string{"*.class"}, "sub/Foo.class", false, true},
	{[]string{"*.class"}, "a/b/c/Foo.class", false, true},
	{[]string{"*.class"}, "Foo.java", false, false},
	{[]string{"*.class"}, "class", false, false},
	{[]string{"*.log"}, "debug.log", false, true},
	{[]string{"*.log"}, "deep/nested/error.log", false, true},
	{[]string{"*.log"}, "logfile", false, false},

	// Dir-only combined with star-suffix.
	{[]string{"*.tmp/"}, "scratch.tmp", true, true},
	{[]string{"*.tmp/"}, "scratch.tmp", false, false},
	{[]string{"*.tmp/"}, "sub/scratch.tmp", true, true},
}

// --- prefix-star (name-*) — basename-only -----------------------------------

var behaviorPrefixStar = []behaviorCase{
	{[]string{"tmp-*"}, "tmp-123", false, true},
	{[]string{"tmp-*"}, "tmp-", false, true}, // empty suffix also matches
	{[]string{"tmp-*"}, "sub/tmp-abc", false, true},
	{[]string{"tmp-*"}, "stmp-abc", false, false},
	{[]string{"tmp-*"}, "TMP-abc", false, false}, // case-sensitive
	{[]string{"cache-*"}, "cache-xyz", false, true},
	{[]string{"cache-*"}, "ycache-xyz", false, false},
}

// --- anchored literal (has slash, no wildcard) ------------------------------

var behaviorAnchoredLiteral = []behaviorCase{
	{[]string{"src/main.go"}, "src/main.go", false, true},
	{[]string{"src/main.go"}, "src/main.go", true, true},
	{[]string{"src/main.go"}, "src/other.go", false, false},
	{[]string{"src/main.go"}, "a/src/main.go", false, false}, // anchored: root only

	// Dir-only anchored.
	{[]string{"src/generated/"}, "src/generated", true, true},
	{[]string{"src/generated/"}, "src/generated", false, false},
	{[]string{"src/generated/"}, "src/generated/foo.go", false, false}, // matcher does NOT walk up
	{[]string{"docs/build/"}, "docs/build", true, true},
}

// --- anchored glob (has slash + wildcard) -----------------------------------

var behaviorAnchoredGlob = []behaviorCase{
	{[]string{"src/*.go"}, "src/main.go", false, true},
	{[]string{"src/*.go"}, "src/a/main.go", false, false}, // * does not cross /
	{[]string{"src/*.go"}, "lib/main.go", false, false},
	{[]string{"src/*.go"}, "src/main.java", false, false},
	{[]string{"a/b/*"}, "a/b/c", false, true},
	{[]string{"a/b/*"}, "a/b/c/d", false, false}, // * stops at /
}

// --- generic globs (character classes, ?) -----------------------------------

var behaviorGenericGlob = []behaviorCase{
	{[]string{"[Mm]akefile"}, "Makefile", false, true},
	{[]string{"[Mm]akefile"}, "makefile", false, true},
	{[]string{"[Mm]akefile"}, "nakefile", false, false},
	{[]string{"f?le.go"}, "file.go", false, true},
	{[]string{"f?le.go"}, "fale.go", false, true},
	{[]string{"f?le.go"}, "fle.go", false, false}, // ? requires exactly one char
	{[]string{"f?le.go"}, "fille.go", false, false},
}

// --- double-star patterns ---------------------------------------------------

var behaviorDoubleStar = []behaviorCase{
	// Leading **/: match basename at any depth.
	{[]string{"**/foo"}, "foo", false, true},
	{[]string{"**/foo"}, "a/foo", false, true},
	{[]string{"**/foo"}, "a/b/c/foo", false, true},
	{[]string{"**/foo"}, "foo.txt", false, false},
	{[]string{"**/foo"}, "afoo", false, false},

	// name/**: everything under name/.
	{[]string{"vendor/**"}, "vendor/a", false, true},
	{[]string{"vendor/**"}, "vendor/a/b/c", false, true},
	{[]string{"vendor/**"}, "vendor", false, true}, // current matcher: prefix alone matches
	{[]string{"vendor/**"}, "other/vendor/a", false, false},

	// **/name/**: name at any depth, with at most ONE further segment.
	// Current matcher splits on the FIRST ** only, so the trailing **
	// degrades to a single * (cannot cross `/`). Pinning the quirk.
	{[]string{"**/node_modules/**"}, "node_modules/x", false, true},
	{[]string{"**/node_modules/**"}, "pkg/node_modules/x", false, true},
	{[]string{"**/node_modules/**"}, "pkg/node_modules/x/y", false, false}, // too deep
	{[]string{"**/node_modules/**"}, "node_modules", false, false},         // needs at least one trailing segment
	{[]string{"**/node_modules/**"}, "foonode_modules/x", false, false},

	// **/*.ext at any depth.
	{[]string{"**/*.generated.go"}, "foo.generated.go", false, true},
	{[]string{"**/*.generated.go"}, "sub/bar.generated.go", false, true},
	{[]string{"**/*.generated.go"}, "a/b/c.generated.go", false, true},
	{[]string{"**/*.generated.go"}, "foo.go", false, false},

	// prefix/**/suffix.
	{[]string{"src/**/main.go"}, "src/main.go", false, true},
	{[]string{"src/**/main.go"}, "src/a/main.go", false, true},
	{[]string{"src/**/main.go"}, "src/a/b/main.go", false, true},
	{[]string{"src/**/main.go"}, "lib/main.go", false, false},

	// Current matcher splits on the FIRST ** only; subsequent **s degrade
	// to a single path.Match * (no depth crossing). Pin this quirk so
	// the trie reproduces it.
	{[]string{"a/**/b/**/c"}, "a/x/b/y/c", false, true},    // one segment between b and c → matches
	{[]string{"a/**/b/**/c"}, "a/x/b/y/z/c", false, false}, // two segments between b and c → fails
}

// --- negation ---------------------------------------------------------------

var behaviorNegation = []behaviorCase{
	// Negation order: user rule appearing after the positive rule un-ignores.
	{[]string{"*.log", "!important.log"}, "important.log", false, false},
	{[]string{"*.log", "!important.log"}, "debug.log", false, true},

	// Negation reversed: order matters — positive rule after negation re-ignores.
	{[]string{"!important.log", "*.log"}, "important.log", false, true},

	// Negation across groups: the matcher iterates base → anchored →
	// doublestar and keeps LAST match across all three. A doublestar
	// positive after an anchored negation wins.
	{[]string{"src/foo.go", "!src/foo.go", "**/foo.go"}, "src/foo.go", false, true},

	// Within-group last-match: two anchored rules, last wins.
	{[]string{"src/foo.go", "!src/foo.go"}, "src/foo.go", false, false},
	{[]string{"!src/foo.go", "src/foo.go"}, "src/foo.go", false, true},

	// Negation on star-suffix.
	{[]string{"*.class", "!keep.class"}, "keep.class", false, false},
	{[]string{"*.class", "!keep.class"}, "foo.class", false, true},

	// Negation on prefix-star.
	{[]string{"tmp-*", "!tmp-keep"}, "tmp-keep", false, false},
	{[]string{"tmp-*", "!tmp-keep"}, "tmp-trash", false, true},
}

// --- cross-group priority (base < anchored < doublestar, last wins) ---------

var behaviorGroupPriority = []behaviorCase{
	// All three match → doublestar wins because it's iterated last.
	{[]string{"!foo.go", "!src/foo.go", "**/foo.go"}, "src/foo.go", false, true},

	// Base + anchored both match, no doublestar → anchored wins.
	{[]string{"!foo.go", "src/foo.go"}, "src/foo.go", false, true},

	// Base + doublestar both match, doublestar is negation → un-ignored.
	{[]string{"foo.go", "!**/foo.go"}, "src/foo.go", false, false},

	// Anchored positive + doublestar negation → un-ignored.
	{[]string{"src/foo.go", "!**/foo.go"}, "src/foo.go", false, false},
}

// --- empty / comment input --------------------------------------------------

var behaviorBlankAndComment = []behaviorCase{
	{[]string{""}, "any.txt", false, false},
	{[]string{"# comment"}, "any.txt", false, false},
	{[]string{"// comment"}, "any.txt", false, false},
	// Blank + real pattern: real pattern applies, blank ignored.
	{[]string{"", "*.log"}, "debug.log", false, true},
	{[]string{"# header", "*.log"}, "debug.log", false, true},
}

// --- dir-only interaction across constructs ---------------------------------

var behaviorDirOnlyAcrossConstructs = []behaviorCase{
	// Dir-only + star-suffix.
	{[]string{"*.dir/"}, "foo.dir", true, true},
	{[]string{"*.dir/"}, "foo.dir", false, false},
	// Dir-only + prefix-star.
	{[]string{"cache-*/"}, "cache-x", true, true},
	{[]string{"cache-*/"}, "cache-x", false, false},
	// Dir-only + anchored literal.
	{[]string{"a/b/"}, "a/b", true, true},
	{[]string{"a/b/"}, "a/b", false, false},
	// Dir-only + doublestar: current matcher still only fires when isDir.
	{[]string{"**/gen/"}, "gen", true, true},
	{[]string{"**/gen/"}, "gen", false, false},
	{[]string{"**/gen/"}, "sub/gen", true, true},
	{[]string{"**/gen/"}, "sub/gen", false, false},
}

// --- combined monorepo gitignore (sanity integration) -----------------------

var behaviorMonorepo = []behaviorCase{
	{
		patterns: []string{
			".git/", ".svn/", ".DS_Store", "Thumbs.db",
			"node_modules/", ".idea/", ".vscode/",
			"*.class", "*.log", "*.tmp", "*.swp",
			"tmp-*", "cache-*",
			"src/generated/", "docs/build/",
			"**/test-output/**", "**/__snapshots__/**",
			"vendor/**",
			"**/*.generated.go",
			"!important.class", "!keep.log",
		},
		path: "src/main/java/App.java", isDir: false, want: false,
	},
	{
		patterns: []string{
			".git/", "*.log", "!important.log",
		},
		path: "sub/debug.log", isDir: false, want: true,
	},
	{
		patterns: []string{
			".git/", "*.log", "!important.log",
		},
		path: "important.log", isDir: false, want: false,
	},
	{
		patterns: []string{
			"**/test-output/**",
		},
		// Current matcher: first ** consumes leading path, second ** degrades
		// to a single * and cannot cross `/`. So one-segment trailers match…
		path: "a/b/test-output/report.html", isDir: false, want: true,
	},
	{
		patterns: []string{
			"**/test-output/**",
		},
		// …and deeper trailers fail (pinned quirk).
		path: "a/b/test-output/c/d/report.html", isDir: false, want: false,
	},
	{
		patterns: []string{
			"vendor/**",
		},
		path: "vendor/github.com/foo/bar.go", isDir: false, want: true,
	},
	{
		patterns: []string{
			"vendor/**",
		},
		path: "other/vendor/x.go", isDir: false, want: false,
	},
}

// --- runner -----------------------------------------------------------------

func runBehaviorTable(t *testing.T, name string, cases []behaviorCase, factory MatcherFactory) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		for i, tc := range cases {
			tc := tc
			title := fmt.Sprintf("%03d/%s_isDir=%v", i, tc.path, tc.isDir)
			t.Run(title, func(t *testing.T) {
				t.Parallel()
				m := factory(tc.patterns)
				got := m(tc.path, tc.isDir)
				if got != tc.want {
					t.Errorf("patterns=%v path=%q isDir=%v: got %v want %v",
						tc.patterns, tc.path, tc.isDir, got, tc.want)
				}
			})
		}
	})
}

// runAllBehaviorTables dispatches every category through the given factory.
// A single test function covers the full corpus so the failure output
// identifies both the category and the exact case that broke.
func runAllBehaviorTables(t *testing.T, factory MatcherFactory) {
	t.Helper()
	runBehaviorTable(t, "builtins", behaviorBuiltins, factory)
	runBehaviorTable(t, "literal_base", behaviorLiteralBase, factory)
	runBehaviorTable(t, "dir_only_literal", behaviorDirOnlyLiteral, factory)
	runBehaviorTable(t, "star_suffix", behaviorStarSuffix, factory)
	runBehaviorTable(t, "prefix_star", behaviorPrefixStar, factory)
	runBehaviorTable(t, "anchored_literal", behaviorAnchoredLiteral, factory)
	runBehaviorTable(t, "anchored_glob", behaviorAnchoredGlob, factory)
	runBehaviorTable(t, "generic_glob", behaviorGenericGlob, factory)
	runBehaviorTable(t, "double_star", behaviorDoubleStar, factory)
	runBehaviorTable(t, "negation", behaviorNegation, factory)
	runBehaviorTable(t, "group_priority", behaviorGroupPriority, factory)
	runBehaviorTable(t, "blank_and_comment", behaviorBlankAndComment, factory)
	runBehaviorTable(t, "dir_only_across_constructs", behaviorDirOnlyAcrossConstructs, factory)
	runBehaviorTable(t, "monorepo", behaviorMonorepo, factory)
}

// TestIgnoreBehaviorLinear pins the current linear matcher against the full
// corpus. This is the contract the trie (and any future implementation) must
// reproduce case-for-case.
func TestIgnoreBehaviorLinear(t *testing.T) {
	runAllBehaviorTables(t, linearFactory)
}

// TestIgnoreBehaviorTrie is the merge gate for PF Phase 2. Every case that
// passes on the linear matcher must produce the identical decision on the
// trie. A failure here is either a trie bug (fix) or a linear quirk the
// trie deliberately departs from (decide with the user; do not flip defaults).
func TestIgnoreBehaviorTrie(t *testing.T) {
	runAllBehaviorTables(t, trieFactory)
}
