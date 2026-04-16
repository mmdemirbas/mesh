package main

import (
	"os"
	"strings"
)

// eastAsianWidth is true when the terminal renders East Asian Ambiguous-width
// characters (○, ●, ◎, ↕, etc.) as 2 columns instead of 1. Detected from
// environment variables and platform-specific APIs at startup.
var eastAsianWidth = detectEastAsian()

func detectEastAsian() bool {
	for _, env := range []string{"RUNEWIDTH_EASTASIAN", "LC_ALL", "LC_CTYPE", "LANG"} {
		v := strings.ToLower(os.Getenv(env))
		if v == "" {
			continue
		}
		if env == "RUNEWIDTH_EASTASIAN" {
			return v == "1" || v == "true" || v == "yes"
		}
		for _, tag := range []string{"zh", "ja", "ko"} {
			if strings.Contains(v, tag) {
				return true
			}
		}
	}
	return detectEastAsianPlatform()
}

// runeWidth returns the terminal display width of r in isolation.
// Variation selectors (VS15/VS16) return 0; visibleLen handles the context.
func runeWidth(r rune) int {
	if r >= 0x1F000 {
		return 2
	}
	if r == 0xFE0F || r == 0xFE0E {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	if eastAsianWidth && isAmbiguousRune(r) {
		return 2
	}
	return 1
}

// isWideRune returns true for characters that are unconditionally 2-wide
// (East Asian Width: W or F) on all terminals.
func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x231A && r <= 0x231B) ||
		(r >= 0x2329 && r <= 0x232A) ||
		(r >= 0x23E9 && r <= 0x23F3) ||
		(r >= 0x25FD && r <= 0x25FE) ||
		(r >= 0x2614 && r <= 0x2615) ||
		(r >= 0x2648 && r <= 0x2653) ||
		(r == 0x267F) ||
		(r == 0x2693) ||
		(r == 0x26A1) ||
		(r >= 0x26AA && r <= 0x26AB) ||
		(r >= 0x26BD && r <= 0x26BE) ||
		(r >= 0x26C4 && r <= 0x26C5) ||
		(r == 0x26CE) ||
		(r == 0x26D4) ||
		(r == 0x26EA) ||
		(r >= 0x26F2 && r <= 0x26F3) ||
		(r == 0x26F5) ||
		(r == 0x26FA) ||
		(r == 0x26FD) ||
		(r == 0x2702) ||
		(r == 0x2705) ||
		(r >= 0x2708 && r <= 0x270D) ||
		(r == 0x270F) ||
		(r == 0x2712) ||
		(r == 0x2714) ||
		(r == 0x2716) ||
		(r == 0x271D) ||
		(r == 0x2721) ||
		(r == 0x2728) ||
		(r >= 0x2733 && r <= 0x2734) ||
		(r == 0x2744) ||
		(r == 0x2747) ||
		(r == 0x274C) ||
		(r == 0x274E) ||
		(r >= 0x2753 && r <= 0x2755) ||
		(r == 0x2757) ||
		(r >= 0x2763 && r <= 0x2764) ||
		(r >= 0x2795 && r <= 0x2797) ||
		(r == 0x27A1) ||
		(r == 0x27B0) ||
		(r == 0x27BF) ||
		(r >= 0x2934 && r <= 0x2935) ||
		(r >= 0x2B05 && r <= 0x2B07) ||
		(r >= 0x2B1B && r <= 0x2B1C) ||
		(r == 0x2B50) ||
		(r == 0x2B55) ||
		(r >= 0x2E80 && r <= 0x303E) ||
		(r >= 0x3040 && r <= 0x33BF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x4E00 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7AF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE19) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF01 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6)
}

// isAmbiguousRune returns true for common East Asian Ambiguous-width characters.
// On CJK terminals these render as 2 columns; on Western terminals as 1.
func isAmbiguousRune(r rune) bool {
	return (r >= 0x2190 && r <= 0x2199) || // Arrows ← ↑ → ↓ ↔ ↕
		(r >= 0x2500 && r <= 0x257F) || // Box Drawing
		(r >= 0x2580 && r <= 0x259F) || // Block Elements
		(r >= 0x25A0 && r <= 0x25FC) || // Geometric Shapes (○ ◎ ● etc.)
		(r >= 0x2600 && r <= 0x2613) || // Misc Symbols
		(r >= 0x2616 && r <= 0x2647) || // Misc Symbols (skip Wide 2614-2615)
		(r >= 0x2660 && r <= 0x267E) || // Misc Symbols (skip Wide 267F)
		(r >= 0x2680 && r <= 0x2692) || // Misc Symbols (skip Wide 2693)
		(r >= 0x2694 && r <= 0x26A0) // Misc Symbols (skip Wide 26A1, 26AA-AB)
}
