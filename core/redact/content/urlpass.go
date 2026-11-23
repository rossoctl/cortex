package content

import (
	"regexp"
	"strings"
)

// THE URL PASSWORD: the password of a URL's user info, "scheme://user:password@host". The scheme,
// user and host stay.
//
// RFC 3986 requires "@ / ? #" to be percent-encoded in a password, but real ones are often not
// ("postgres://bob:p@ss/w0rd@db"). So the password runs to the LAST "@" that a host follows,
// within the user info's bounded run, and may hold "@", "/" and "#".
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
		if u < len(text) && text[u] == '[' {
			continue // a bracketed IPv6 host comes first: there is no user info
		}
		end := u
		for end < len(text) && end-u < maxUserInfo && isUserInfoByte(text[end]) {
			end++
		}
		run := text[u:end]
		// A list of URLs joined by "," or ";" has no space between them: the run stops where
		// the next URL's scheme starts, so an "@" of the next URL is never this one's.
		nextURL := false
		if k := strings.Index(run, "://"); k >= 0 {
			run, nextURL = run[:schemeStart(run, k)], true
		}
		firstAt := strings.IndexByte(run, '@')
		if firstAt < 0 {
			// No "@": nor in the run of any later trigger that starts inside this one and ends
			// where it ends, so those are skipped, which keeps a long run of "a://b:c" linear.
			// A run cut at maxUserInfo may continue past the cut for a later trigger, so then
			// only its first half is skipped. A run cut at the next URL skips to it.
			switch {
			case nextURL:
				covered = u + len(run)
			case end-u < maxUserInfo:
				covered = end
			default:
				covered = u + maxUserInfo/2
			}
			continue
		}
		colon := strings.IndexByte(run[:firstAt], ':')
		if colon < 0 || colon > 64 || strings.ContainsAny(run[:colon], "/#") {
			continue
		}
		last := lastHostAt(run, colon)
		if last < 0 {
			continue
		}
		pw := run[colon+1 : last]
		if len(pw) == 0 || isPortPath(pw) || strings.Contains(pw, "://") || strings.HasPrefix(pw, "[REDACTED:") {
			continue
		}
		out = append(out, Edit{Start: u + colon + 1, End: u + last, Cat: catPassword})
		covered = u + last
	}
	return out
}

// lastHostAt returns the position in run of the last "@" after colon that a host follows, or -1.
// Taking the last one keeps a password that holds "@" and "/" whole ("p@ss/w0rd@db"). If the
// URL also has an email in its path after real credentials, more than the password is replaced:
// over-redaction, the safe side.
//
// An "@" right after "/" is usually a path segment (an npm scope, "/@babel/core"), so one that is
// not is preferred. When "/@" is the only one, it separates a password that ends in "/" from its
// host ("u:s3cr3t/@db"), and is taken. It is taken too when what follows the earlier "@" is not
// host-like (no "." or ":"): in "u:p@ss/@db", "ss" is part of the password "p@ss/", while in
// "u:p@registry.npmjs.org/@babel/core" the host stays. A dotless host before a "/@" path
// ("u:p@localhost/@x") then loses its name too: over-redaction, the safe side.
func lastHostAt(run string, colon int) int {
	scoped := -1
	for i := len(run) - 1; i > colon; i-- {
		if run[i] == '@' && isHostAt(run[i+1:]) {
			if run[i-1] != '/' {
				if scoped >= 0 && !hostLike(run[i+1:scoped]) {
					return scoped
				}
				return i
			}
			if scoped < 0 {
				scoped = i
			}
		}
	}
	return scoped
}

// hostLike reports whether s, up to the end of its authority ("/", "?", "#"), holds a "." or a ":"
// (a domain, an IP address, a port), as a host does and a word of a password usually does not.
func hostLike(s string) bool {
	if j := strings.IndexAny(s, "/?#"); j >= 0 {
		s = s[:j]
	}
	return strings.ContainsAny(s, ".:")
}

// isHostAt reports whether rest starts with a host: one or more host characters, up to the end
// of the authority ("/", "?", "#"), the separator of a list of URLs ("," ";"), or the run's end.
// Any other byte means it is not a host, so an "@" in a path ("/x@y=z", "/pkg@1.0+local") is not
// taken for the host's, and the real host stays. ("|" is not a separator: in a shell command it
// is a pipe, "…/b@v1|grep x".)
func isHostAt(rest string) bool {
	if rest == "" || !isHostStart(rest[0]) {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if strings.IndexByte("/?#,;", c) >= 0 {
			return true
		}
		if !(isLetter(c) || c >= '0' && c <= '9' || strings.IndexByte(".-:[]_%", c) >= 0) {
			return false
		}
	}
	return true
}

func isHostStart(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' || c == '[' }

// isPortPath reports whether a password candidate is really a port followed by a path, a
// fragment or the next URL of a list ("9200,http://elastic@es2:9200"): all digits up to its
// first "/", "#" or ",", and there is one.
func isPortPath(pw string) bool {
	i := strings.IndexAny(pw, "/#,")
	return i > 0 && strings.Trim(pw[:i], "0123456789") == ""
}
