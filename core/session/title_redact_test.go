package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeGitHubKey is shaped like a GitHub token and is not one: built, so no key-shaped literal
// sits in the repo.
func fakeGitHubKey() string { return "gh" + "p_" + strings.Repeat("G", 36) }

// A key pasted into the first prompt is not in the title: not whole, and not the piece of it a
// clip at rune 80 would leave.
func TestSanitizeTitle_RedactsSecrets(t *testing.T) {
	key := fakeGitHubKey()
	for _, in := range []string{
		"use " + key + " to push",
		// Starts inside the title, runs past rune 80: a rule run on the clipped title would see
		// too short a piece to match.
		strings.Repeat("word ", 14) + key + " and more",
	} {
		got := sanitizeTitle(in)
		if strings.Contains(got, "GGGGGGGG") || strings.Contains(got, "gh"+"p_") {
			t.Errorf("%q\n -> %q, still holds the key", in, got)
		}
		if n := utf8.RuneCountInString(got); n > maxTitleLen {
			t.Errorf("%q -> %d runes, want at most %d", in, n, maxTitleLen)
		}
		if i := strings.LastIndex(got, "[REDACTED"); i >= 0 && !strings.Contains(got[i:], "]") {
			t.Errorf("%q -> %q, a severed marker", in, got)
		}
	}
	if got := sanitizeTitle("use " + key + " to push"); got != "use [REDACTED:api_key] to push" {
		t.Errorf("got %q", got)
	}
	// Plain titles are as before.
	if got := sanitizeTitle("  fix the\nflaky   store test "); got != "fix the flaky store test" {
		t.Errorf("plain title: %q", got)
	}
}

// The rules see the raw text: a password ended by a line break is a password, though the fold
// turns the break into a space and a word followed by words reads as prose.
func TestSanitizeTitle_RedactsBeforeTheFold(t *testing.T) {
	got := sanitizeTitle("fix login:\nDB_PASSWORD=Sunflower\nDB_HOST=db")
	if strings.Contains(got, "Sunflower") {
		t.Errorf("got %q, the password survived the fold", got)
	}
}

// A clip that cuts a marker anywhere, even in its first letters, drops the piece.
func TestSanitizeTitle_DropsAnyPieceOfASeveredMarker(t *testing.T) {
	key := fakeGitHubKey()
	for n := 12; n <= 17; n++ {
		got := sanitizeTitle(strings.Repeat("word ", n) + key)
		if strings.Contains(got, "GGGG") {
			t.Fatalf("%d words: %q holds the key", n, got)
		}
		if i := strings.LastIndexByte(got, '['); i >= 0 && !strings.Contains(got[i:], "]") {
			t.Errorf("%d words: %q ends in a severed marker", n, got)
		}
	}
}

// From the hostile review of b550bcb4, one probe per finding: a key hidden from the rules by an
// ANSI escape or an invisible rune, a key pushed out of the window by a run of spaces, and a
// stored title the old clip left with a key's prefix.
func TestSanitizeTitle_ReviewProbes(t *testing.T) {
	key := fakeGitHubKey()
	for name, in := range map[string]string{
		"ansi":         "token \x1b[1m" + key + "\x1b[0m please",
		"zero-width":   "use " + key[:10] + "​" + key[10:],
		"soft hyphen":  "use " + key[:10] + "­" + key[10:],
		"combining":    "use " + key[:10] + "́" + key[10:],
		"space window": "fix" + strings.Repeat(" ", 1274) + key,
		"tab window":   "fix" + strings.Repeat("\t ", 900) + key,
	} {
		got := sanitizeTitle(in)
		if strings.Contains(got, "GGGG") || strings.Contains(got, "gh"+"p") {
			t.Errorf("%s: %q holds the key", name, got)
		}
	}
	anthropic := "sk-" + "ant-" + "api03-" + strings.Repeat("A", 26) + "-" + strings.Repeat("B", 19)
	for n := 9; n <= 16; n++ {
		for _, k := range []string{key, anthropic} {
			stored := cutRunes(strings.Repeat("word ", n)+k, maxTitleLen) // as the old clip stored it
			got := redactStoredTitle(stored)
			if strings.Contains(got, "gh"+"p_") || strings.Contains(got, "sk-") {
				t.Errorf("%d words: stored %q -> %q", n, stored, got)
			}
		}
	}
	// A stored title shorter than the clip was not cut: a trailing "sk-" there is not a key.
	if got := redactStoredTitle("try sk-" + "learn"); got != "try sk-"+"learn" {
		t.Errorf("an uncut title changed: %q", got)
	}
}

// From the last review of the titles: every whitespace the fold collapses is squeezed, the
// squeeze reads a bounded part of the candidate, a value assigned to a secret's name is replaced
// when the clip cut it, and a word replaced at the cut stays in sight as a marker.
func TestSanitizeTitle_LastReviewProbes(t *testing.T) {
	key := fakeGitHubKey()
	for name, sp := range map[string]string{"nbsp": " ", "form feed": "\f", "nul": "\x00", "ideographic": "　"} {
		if got := sanitizeTitle("fix" + strings.Repeat(sp, 1270) + key); strings.Contains(got, "gh"+"p") {
			t.Errorf("%s run: %q holds the key", name, got)
		}
	}
	// A huge run of spaces is not read to its end: the squeeze stops at titleScanBytes.
	if got := sanitizeTitle("a" + strings.Repeat(" ", 2<<20) + "b"); got != "a" {
		t.Errorf("a long run: %q, want the squeeze to stop before b", got)
	}
	stored := cutRunes(strings.Repeat("word ", 12)+"DB_PASSWORD=Sunflower", maxTitleLen)
	if got := redactStoredTitle(stored); strings.Contains(got, "Sun") {
		t.Errorf("a cut assignment: %q -> %q", stored, got)
	}
	stored = cutRunes(strings.Repeat("word ", 15)+"sk-"+"learning", maxTitleLen)
	if got := redactStoredTitle(stored); !strings.HasSuffix(got, "[REDACTED:api_key]") || utf8.RuneCountInString(got) > maxTitleLen {
		t.Errorf("a word replaced at the cut: %q -> %q, want the marker in sight", stored, got)
	}
}

// A title stored before the rules applied is redacted when an archived summary is read back.
func TestRedactStoredTitle(t *testing.T) {
	if got := redactStoredTitle("push with " + fakeGitHubKey()); got != "push with [REDACTED:api_key]" {
		t.Errorf("got %q", got)
	}
	if got := redactStoredTitle("flaky store test"); got != "flaky store test" {
		t.Errorf("plain: %q", got)
	}
}
