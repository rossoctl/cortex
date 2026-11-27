package claude

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeKey is shaped like a GitHub token and is not one: built, so no key-shaped literal sits in
// the repo.
func fakeKey() string { return "gh" + "p_" + strings.Repeat("G", 36) }

// A harvested title holds no pasted key: not whole, and not the piece a cut at MaxTitleLen leaves.
func TestClipTitle_RedactsSecrets(t *testing.T) {
	key := fakeKey()
	for _, in := range []string{"push with " + key, strings.Repeat("word ", 14) + key + " tail"} {
		got := clipTitle(in)
		if strings.Contains(got, "GGGGGGGG") || strings.Contains(got, "gh"+"p_") {
			t.Errorf("%q -> %q, still holds the key", in, got)
		}
		if utf8.RuneCountInString(got) > MaxTitleLen {
			t.Errorf("%q -> %q, over MaxTitleLen", in, got)
		}
		if i := strings.LastIndex(got, "[REDACTED"); i >= 0 && !strings.Contains(got[i:], "]") {
			t.Errorf("%q -> %q, a severed marker", in, got)
		}
	}
	if got := clipTitle("push with " + key); got != "push with [REDACTED:api_key]" {
		t.Errorf("got %q", got)
	}
}

// The rules see the raw text, before normalizing turns a line break into a space; and a marker
// the cut severs anywhere is dropped.
func TestClipTitle_RedactsBeforeNormalizingAndDropsSeveredMarkers(t *testing.T) {
	if got := clipTitle("fix login:\nDB_PASSWORD=Sunflower\nDB_HOST=db"); strings.Contains(got, "Sunflower") {
		t.Errorf("got %q, the password survived normalizing", got)
	}
	for n := 12; n <= 17; n++ {
		got := clipTitle(strings.Repeat("word ", n) + fakeKey())
		if strings.Contains(got, "GGGG") {
			t.Fatalf("%d words: %q holds the key", n, got)
		}
		if i := strings.LastIndexByte(got, '['); i >= 0 && !strings.Contains(got[i:], "]") {
			t.Errorf("%d words: %q ends in a severed marker", n, got)
		}
	}
}

// From the hostile review of b550bcb4, one probe per finding, on the harvest's side.
func TestClipTitle_ReviewProbes(t *testing.T) {
	key := fakeKey()
	for name, in := range map[string]string{
		"ansi":         "token \x1b[1m" + key + "\x1b[0m please",
		"zero-width":   "use " + key[:10] + "​" + key[10:],
		"soft hyphen":  "use " + key[:10] + "­" + key[10:],
		"combining":    "use " + key[:10] + "́" + key[10:],
		"space window": "fix" + strings.Repeat(" ", 64<<10-20) + key,
	} {
		got := clipTitle(in)
		if strings.Contains(got, "GGGG") || strings.Contains(got, "gh"+"p") {
			t.Errorf("%s: %q holds the key", name, got)
		}
	}
	for n := 9; n <= 16; n++ {
		r := []rune(strings.Repeat("word ", n) + key)
		stored := string(r[:min(len(r), MaxTitleLen)]) // as the old clip stored it
		m := map[string]SessionMetadata{"s": {Title: stored}}
		RedactStoredTitles(m)
		if strings.Contains(m["s"].Title, "gh"+"p_") {
			t.Errorf("%d words: stored %q -> %q", n, stored, m["s"].Title)
		}
	}
}

// Titles harvested before the rules applied are redacted as the file is read; the rest of an
// entry, and a clean title, are left as they were.
func TestRedactStoredTitles(t *testing.T) {
	m := map[string]SessionMetadata{
		"a": {Title: "push with " + fakeKey(), LogFile: "/x/a.jsonl"},
		"b": {Title: "flaky store test"},
		"c": {},
	}
	RedactStoredTitles(m)
	if m["a"].Title != "push with [REDACTED:api_key]" || m["a"].LogFile != "/x/a.jsonl" {
		t.Errorf("a = %+v", m["a"])
	}
	if m["b"].Title != "flaky store test" || m["c"].Title != "" {
		t.Errorf("b = %+v, c = %+v", m["b"], m["c"])
	}
}
