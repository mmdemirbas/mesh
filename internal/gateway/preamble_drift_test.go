package gateway

import (
	"regexp"
	"strings"
	"testing"
)

// legacyPreambleTagOpen is a verbatim copy of the regex that lived in
// cmd/mesh/audit_stats.go's customTagOpen variable before the
// gateway-side consolidation in 7b₂. Kept here as a sealed reference
// for the drift safety net — never touched by production code.
var legacyPreambleTagOpen = regexp.MustCompile(`(?i)<([a-z][a-z0-9-]{2,40})\b[^>]*>`)

// legacyPreambleBlock and legacyExtractPreambleBlocks reproduce the
// historical cmd/mesh scanPreambleTags / extractPreamblePayloads
// behavior verbatim. The signature matches gateway.PreambleBlock
// so the cross-impl test can compare slices directly.
type legacyPreambleBlock struct {
	Name string
	Body string
}

// legacyExtractPreambleBlocks is the verbatim port of cmd/mesh
// scanPreambleTags as of pre-7b₂. The intent is "produce identical
// output to what the admin UI's preamble rollup used to compute, on
// every input." If gateway.ExtractPreambleBlocks ever diverges from
// this reference, the drift test fails.
//
// Keep this function frozen. Any change to the reference invalidates
// the safety net — if the spec for "what is preamble" needs to
// evolve, evolve gateway.ExtractPreambleBlocks AND the snapshot
// fixtures in TestExtractPreambleBlocks_DriftAgainstLegacy below.
// Don't tweak this reference to match a new implementation; it
// exists precisely to catch silent semantic shifts.
func legacyExtractPreambleBlocks(s string) []legacyPreambleBlock {
	type matched struct {
		name               string
		start, end         int
		bodyStart, bodyEnd int
	}
	opens := legacyPreambleTagOpen.FindAllStringSubmatchIndex(s, -1)
	if len(opens) == 0 {
		return nil
	}
	var tags []matched
	for _, om := range opens {
		name := strings.ToLower(s[om[2]:om[3]])
		closeTag := "</" + name + ">"
		tail := s[om[1]:]
		idx := strings.Index(strings.ToLower(tail), closeTag)
		if idx < 0 {
			continue
		}
		tags = append(tags, matched{
			name:      name,
			start:     om[0],
			end:       om[1] + idx + len(closeTag),
			bodyStart: om[1],
			bodyEnd:   om[1] + idx,
		})
	}
	if len(tags) == 0 {
		return nil
	}
	inBlock := func(i int) bool {
		for _, t := range tags {
			if i >= t.start && i < t.end {
				return true
			}
		}
		return false
	}
	firstTyped, lastTyped := -1, -1
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if inBlock(i) {
			continue
		}
		if firstTyped == -1 {
			firstTyped = i
		}
		lastTyped = i
	}
	out := make([]legacyPreambleBlock, 0, len(tags))
	for _, t := range tags {
		if firstTyped != -1 && t.start >= firstTyped && t.end <= lastTyped+1 {
			continue
		}
		out = append(out, legacyPreambleBlock{Name: t.name, Body: s[t.bodyStart:t.bodyEnd]})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TestExtractPreambleBlocks_DriftAgainstLegacy is the load-bearing
// invariant: for every input, gateway.ExtractPreambleBlocks must
// produce output equivalent to the legacy cmd/mesh implementation.
// "Equivalent" = same length, same Name and Body in the same order.
//
// Cases cover the shapes observed in real Claude Code traffic
// (system-reminder + command-name + command-args at the leading
// edge; tool-result wrappers; nested mismatched tags) and a few
// edge constructions chosen to stress the firstTyped/lastTyped
// boundary detection.
//
// To add coverage: append cases. Don't tweak existing strings unless
// you've hand-verified the legacy output for the new spelling.
func TestExtractPreambleBlocks_DriftAgainstLegacy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"only whitespace", "   \n\t  "},
		{"no tags at all", "Hello, please read main.go and tell me what it does."},
		{"single leading system-reminder", "<system-reminder>be terse</system-reminder>list files"},
		{"single trailing tool-result wrapper",
			"What does main.go do?<tool-results>file contents...</tool-results>"},
		{"two leading tags + typed body",
			"<system-reminder>r1</system-reminder><command-name>/test</command-name>actually do this thing"},
		{"three tags surrounding typed body",
			"<command-name>/init</command-name><command-args>--force</command-args>please run it<env>local</env>"},
		{"embedded tag in middle of typed prose (NOT preamble)",
			"Read the file <tag>quoted bit</tag> and report back."},
		{"leading + embedded mix — only leading is preamble",
			"<system-reminder>r</system-reminder>I want <tag>this</tag> done please"},
		{"unclosed tag — ignored",
			"<system-reminder>no close...still typing here"},
		{"case-mixed close tag matches",
			"<System-Reminder>x</SYSTEM-REMINDER>typed"},
		{"tag with attributes",
			`<command-name kind="builtin">/test</command-name>do it`},
		{"tag name too short to match (1 char) — not preamble",
			"<a>x</a>do something"},
		{"tag name long enough — preamble",
			"<system-reminder>x</system-reminder>do"},
		{"only tags, no typed content — every tag is preamble",
			"<system-reminder>r1</system-reminder><command-name>/x</command-name>"},
		{"trailing tag only",
			"please do<system-reminder>note</system-reminder>"},
		{"adjacent typed-and-tag with no whitespace gap",
			"hi<system-reminder>r</system-reminder>bye"},
		{"newline between leading tag and typed text",
			"<system-reminder>r</system-reminder>\n\nactually run it"},
		{"unicode in body",
			"<system-reminder>böse</system-reminder>тест"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractPreambleBlocks(c.in)
			want := legacyExtractPreambleBlocks(c.in)
			if !sameLegacyShape(got, want) {
				t.Errorf("drift: input=%q\n  got = %s\n  want = %s",
					c.in, formatBlocks(got), formatLegacy(want))
			}
		})
	}
}

func sameLegacyShape(got []PreambleBlock, want []legacyPreambleBlock) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Name != want[i].Name || got[i].Body != want[i].Body {
			return false
		}
	}
	return true
}

func formatBlocks(blocks []PreambleBlock) string {
	if len(blocks) == 0 {
		return "<none>"
	}
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = "{" + b.Name + ":" + b.Body + "}"
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func formatLegacy(blocks []legacyPreambleBlock) string {
	if len(blocks) == 0 {
		return "<none>"
	}
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = "{" + b.Name + ":" + b.Body + "}"
	}
	return "[" + strings.Join(parts, " ") + "]"
}
