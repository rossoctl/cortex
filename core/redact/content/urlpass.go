package content

import (
	"regexp"
	"strings"
)

// THE URL PASSWORD: the password of a URL's user info, "scheme://user:password@host". The scheme,
// user and host stay.
//
// RFC 3986 requires "@ / ? #" to be percent-encoded in a password, but real ones are often not
// ("postgres://bob:p@ss/w0rd@db"). So the password runs to the LAST "@" before the authority's
// end, which is the first "/" after the first "@". A password may then hold "@", "/" and "#".
// It never holds whitespace, a quote, a backslash or "?", so a match cannot run from a URL into
// the next JSON value or the next word. A "password" that starts with a port ("8080/x#a@b",
// "http://host:8080/contact#ops@example.com") is a port, a path and an email, and is kept; so
// is one holding "://", which is the next URL of a list.

var urlSchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// isUserInfoByte reports whether c can be part of a URL's user info, as this rule reads it.
func isUserInfoByte(c byte) bool {
	return c > ' ' && c != '"' && c != '\'' && c != '\\' && c != '?' && c != '<' && c != '>' && c != '`' && c != 0x7f // UTF-8 too
}

const maxUserInfo = 256

// findURLPasswords runs the URL password rule at each "://" (sorted).
func findURLPasswords(text string, at []int, out []Edit) []Edit {
	covered := 0
	for _, p := range at {
		if p < covered {
			continue
		}
		s := schemeStart(text, p)
		if m := urlSchemePattern.FindStringIndex(text[s:min(len(text), p+3)]); m == nil || s+m[1] != p+3 {
			continue
		}
		u := p + 3
		end := u
		for end < len(text) && end-u < maxUserInfo && isUserInfoByte(text[end]) {
			end++
		}
		run := text[u:end]
		colon := strings.IndexByte(run, ':')
		firstAt := strings.IndexByte(run, '@')
		if colon < 0 || firstAt < 0 || colon > firstAt || strings.ContainsAny(run[:colon], "/#@") || colon > 64 {
			continue
		}
		authEnd := len(run)
		if i := strings.IndexByte(run[firstAt:], '/'); i >= 0 {
			authEnd = firstAt + i
		}
		last := strings.LastIndexByte(run[:authEnd], '@')
		pw := run[colon+1 : last]
		if len(pw) == 0 || len(pw) > 128 || last+1 >= len(run) || !isHostStart(run[last+1]) {
			continue
		}
		if isPortPath(pw) || strings.Contains(pw, "://") || strings.HasPrefix(pw, "[REDACTED:") {
			continue
		}
		out = append(out, Edit{Start: u + colon + 1, End: u + last, Cat: catPassword})
		covered = u + last
	}
	return out
}

func isHostStart(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' || c == '[' }

// isPortPath reports whether a password candidate is really a port followed by a path, a
// fragment or the next URL of a list ("9200,http://elastic@es2:9200"): all digits up to its
// first "/", "#" or ",", and there is one.
func isPortPath(pw string) bool {
	i := strings.IndexAny(pw, "/#,")
	return i > 0 && strings.Trim(pw[:i], "0123456789") == ""
}
