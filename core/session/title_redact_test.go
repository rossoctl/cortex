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

// A title stored before the rules applied is redacted when an archived summary is read back.
func TestRedactStoredTitle(t *testing.T) {
	if got := redactStoredTitle("push with " + fakeGitHubKey()); got != "push with [REDACTED:api_key]" {
		t.Errorf("got %q", got)
	}
	if got := redactStoredTitle("flaky store test"); got != "flaky store test" {
		t.Errorf("plain: %q", got)
	}
}
