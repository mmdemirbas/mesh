package gateway

import (
	"regexp"
	"strings"
)

// PreambleBlock is one injected pseudo-XML block (`<system-reminder>`,
// `<command-name>`, etc.) extracted from a user-role message body.
// Body is the raw text between the open and close tags, with the
// tags themselves excluded.
type PreambleBlock struct {
	Name string
	Body string
}

// preambleTagOpen matches the opening of a pseudo-XML preamble block
// like <system-reminder> or <command-name ...>. RE2 has no
// backreferences, so the matching close tag is found by hand-scanning
// in ExtractPreambleBlocks.
var preambleTagOpen = regexp.MustCompile(`(?i)<([a-z][a-z0-9-]{2,40})\b[^>]*>`)

// matchedTag records one preamble open/close pair plus the body span.
type matchedTag struct {
	name               string
	start, end         int // full tag range in s, including <name> and </name>
	bodyStart, bodyEnd int // the text between the tags
}

// ExtractPreambleBlocks scans s for pseudo-XML preamble blocks at the
// leading and trailing edges of typed content, returning every
// (name, body) pair. Blocks in the middle of typed prose are treated
// as intentional user quotes and excluded.
//
// "Edge" means: a tag whose span lies fully outside the
// [firstTyped, lastTyped+1) range, where firstTyped/lastTyped are the
// positions of the first and last non-whitespace characters that are
// NOT inside any other tag's span. When the message contains no
// typed content (only tags + whitespace), every tag is preamble.
//
// This is the canonical preamble extractor for the gateway: both the
// section-byte partition (`splitTextAndPreamble`) and the admin-stats
// rollup call this function. See preamble_drift_test.go for the
// invariant check against the historical `cmd/mesh` implementation.
func ExtractPreambleBlocks(s string) []PreambleBlock {
	tags := matchPreambleTags(s)
	if len(tags) == 0 {
		return nil
	}
	firstTyped, lastTyped := typedSpan(s, tags)
	out := make([]PreambleBlock, 0, len(tags))
	for _, t := range tags {
		if firstTyped != -1 && t.start >= firstTyped && t.end <= lastTyped+1 {
			// Tag is entirely embedded in typed prose — treat as a
			// user quote, not preamble.
			continue
		}
		out = append(out, PreambleBlock{
			Name: t.name,
			Body: s[t.bodyStart:t.bodyEnd],
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchPreambleTags returns every well-formed (open, close) tag pair
// in s. Open tags without a matching close are skipped.
func matchPreambleTags(s string) []matchedTag {
	opens := preambleTagOpen.FindAllStringSubmatchIndex(s, -1)
	if len(opens) == 0 {
		return nil
	}
	tags := make([]matchedTag, 0, len(opens))
	for _, om := range opens {
		name := strings.ToLower(s[om[2]:om[3]])
		close := "</" + name + ">"
		tail := s[om[1]:]
		idx := strings.Index(strings.ToLower(tail), close)
		if idx < 0 {
			continue
		}
		tags = append(tags, matchedTag{
			name:      name,
			start:     om[0],
			end:       om[1] + idx + len(close),
			bodyStart: om[1],
			bodyEnd:   om[1] + idx,
		})
	}
	return tags
}

// typedSpan returns the byte offsets of the first and last
// non-whitespace, non-in-tag character. Both are -1 when the input
// has no typed content outside tags. Used by ExtractPreambleBlocks
// and by splitTextAndPreamble to decide which tags are "edge"
// (preamble) vs "embedded" (user quote).
func typedSpan(s string, tags []matchedTag) (firstTyped, lastTyped int) {
	firstTyped, lastTyped = -1, -1
	inBlock := func(i int) bool {
		for _, t := range tags {
			if i >= t.start && i < t.end {
				return true
			}
		}
		return false
	}
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
	return firstTyped, lastTyped
}
