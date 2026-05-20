package tui

import (
	"fmt"
	"strings"
)

// formatCount renders an integer with thousands separators so large
// numbers stay readable in narrow table columns. The sign is preserved
// for negatives (e.g. -1234 -> "-1,234"); zero and single-triplet values
// are passed through unchanged.
func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	// Split the sign off before grouping so a leading "-" doesn't get
	// counted as a digit in the triplet math (otherwise len("-abc") % 3
	// == 1 produced a bogus "-,abc" prefix for 3/6/9-digit negatives).
	sign := ""
	if len(s) > 0 && s[0] == '-' {
		sign = "-"
		s = s[1:]
	}
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return sign + b.String()
}

// nonEmpty returns s if non-empty, otherwise fallback. Used by label
// renderers where we want an em-dash sentinel in place of empty strings.
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// plural returns "s" when n != 1, "" otherwise. Lets footer hints write
// "1 skip hidden" / "2 skips hidden" without a separate format-string
// branch per case.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
