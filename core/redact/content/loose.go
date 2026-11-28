package content

import (
	"regexp"
	"unicode"
	"unicode/utf8"
)

// SKIPPING INVISIBLE RUNES, for text that is shown cleaned (a session title): an ANSI escape
// sequence, a format character (a zero-width space, a soft hyphen, a bidi mark) or a combining
// mark inside a secret hides it from the rules, and a cleaner that strips them afterwards puts it
// back together: "ghp_" + "\x1b[1m" + the rest reads as two pieces here and as one key on screen.

// StringSkipping is String for such text: the rules run on s with those runes left out, and each
// secret found is replaced in s whole, the runes inside it included. Text with none of them is
// redacted as String does.
func (r *Redactor) StringSkipping(s string) (string, Stats) {
	probe, at := skipInvisible(s)
	if at == nil {
		return r.String(s)
	}
	edits := r.Find(probe, Context{})
	if len(edits) == 0 {
		return s, Stats{}
	}
	mapped := make([]Edit, len(edits))
	for i, e := range edits {
		end := len(s)
		if e.End < len(probe) {
			end = at[e.End]
		}
		mapped[i] = Edit{Start: at[e.Start], End: end, Cat: e.Cat}
	}
	return Apply(s, mapped), Count(mapped)
}

// skipInvisible returns s without its ANSI escape sequences, format characters and combining
// marks, and, for each byte of the result, its offset in s; a nil map when there were none.
func skipInvisible(s string) (string, []int) {
	var out []byte
	var at []int
	skipped := false
	for i := 0; i < len(s); {
		if n := ansiLen(s[i:]); n > 0 {
			skipped = true
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			skipped = true
			i += size
			continue
		}
		for k := 0; k < size; k++ {
			out = append(out, s[i+k])
			at = append(at, i+k)
		}
		i += size
	}
	if !skipped {
		return s, nil
	}
	return string(out), at
}

// ansiLen is the length of the ANSI escape sequence s starts with, or 0: a CSI ("ESC [", then
// parameters, then a final byte in @-~) or an OSC ("ESC ]" to BEL or "ESC \").
func ansiLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 0
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s) && i < 64; i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
	case ']':
		for i := 2; i < len(s) && i < 512; i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
	}
	return 0
}

// keyPrefixes are the known prefixes of the provider keys in apiKeys (rules.go), and a JWT's
// "eyJ": kept in step with that list.
const keyPrefixes = `gh[pousr]_|github_pat_|sk-|[sr]k_(?:live|test)_|AKIA|ASIA|ABIA|ACCA|AIza|xox[abeprs][.-]|xapp-|` +
	`glpat-|gldt-|hf_|npm_|pypi-|SG\.|dapi|eyJ`

// keyFragmentAtEnd is a provider key's known prefix and what follows it, at the end of a text: a
// key a clip cut short, too short for its rule to find.
var keyFragmentAtEnd = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])((?:` + keyPrefixes + `)[A-Za-z0-9_.-]*)$`)

// keyPrefixAtStart matches a known key prefix at the start of a text.
var keyPrefixAtStart = regexp.MustCompile(`^(?:` + keyPrefixes + `)`)

// KeyPrefixAt reports whether a known provider key's prefix starts s: where a cleaner dropped a
// rune just before it, a reader sees the key start there, glued to the word before.
func KeyPrefixAt(s string) bool { return keyPrefixAtStart.MatchString(s) }

// assignmentAtEnd is a name, an assignment and the start of a value, at the end of a text:
// "DB_PASSWORD=Sun" a clip cut short.
var assignmentAtEnd = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])([A-Za-z_][A-Za-z0-9_.-]*)["']?[ \t]*(?::=|[:=])[ \t]*["']?([^\s"']+)$`)

// RedactCutKey replaces what a clip cut short at the end of s and is too short for its rule: a
// key's known prefix and what follows it, with the api_key marker, or the start of a value
// assigned to a secret's name ("DB_PASSWORD=Sun"), with its tier's marker. Only for text known to
// be cut there: in whole text, "sk-" and "eyJ" at the end are not a key.
func RedactCutKey(s string) (string, bool) {
	if m := keyFragmentAtEnd.FindStringSubmatchIndex(s); m != nil {
		return s[:m[2]] + marker(catAPIKey), true
	}
	if m := assignmentAtEnd.FindStringSubmatchIndex(s); m != nil {
		if t := nameTier(s[m[2]:m[3]]); t != tierNone {
			return s[:m[4]] + marker(t.cat()), true
		}
	}
	return s, false
}
