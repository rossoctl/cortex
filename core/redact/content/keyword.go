package content

import (
	"math"
	"regexp"
	"strings"
)

// THE KEYWORD RULE: a secret-named key assigned a secret-shaped value, in text ("password=x",
// "AWS_SECRET_ACCESS_KEY: x", `"client_secret": "x"`, `\"token\":\"x\"` in JSON inside a JSON
// string), or a value whose JSON key the caller passes (Context.Key). Only the value is
// replaced; the key and the quotes stay.
//
// The name decides the tier, and the tier how secret-shaped the value must be:
//   - password names (password, passwd, pwd, pass, passphrase): 6 or more characters, no entropy
//     floor, since a human-chosen password has little;
//   - secret names (secret, token, credential, auth, cookie, api key, access key, ...): a value
//     shaped like a generated token (see tokenShaped), or a UUID;
//   - names whose only secret word is "key" (cache_key, Idempotency-Key): a token-shaped value
//     of 20 or more characters with no ":" or "."; hex and UUIDs are kept, as hashes and ids.
//
// Every tier keeps placeholders, references and known ids: "${DB_PASSWORD}", "changeme",
// "self.password", "/home/user", tool-use and message ids.

// keywordTriggers are the words a secret name holds, lowercase. A name is found from one of its
// words, then classified whole.
var keywordTriggers = []string{"pass", "pwd", "secret", "token", "credential", "auth", "cookie", "key"}

// valueChars is a bare value's character: no whitespace, quote, backtick, backslash, or the
// punctuation that ends a value in JSON, code, a shell or a query string.
const valueChars = "[^\\s'\"`,;&(){}\\[\\]<>\\\\]"

// keywordPattern is a name, an optional closing quote (straight or escaped), an assignment
// (":", "=", ":=" or "=>") on the same line, and the value: escaped-double-quoted,
// double-quoted, single-quoted or bare. RE2 has no backreference to match a closing quote to its
// opening one, so the four quotings are four alternatives; the value is group 2, 3, 4 or 5. A
// quoted value runs to its closing quote, spaces and punctuation included; it cannot hold a
// backslash or a line break, so it cannot run out of its JSON string.
var keywordPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.-]*)(?:\\?["'])?[ \t]*(?::=|=>|[:=])[ \t]*` +
	`(?:\\"([^"\\\r\n]+)\\"|"([^"\\\r\n]+)"|'([^'\\\r\n]+)'|(` + valueChars + `+))`)

type tier int

const (
	tierNone tier = iota
	tierKey
	tierSecret
	tierPassword
)

func (t tier) cat() string {
	if t == tierPassword {
		return catPassword
	}
	return catSecretValue
}

// valueMode is how a value was found, which decides whether it may hold spaces.
type valueMode int

const (
	valueBare   valueMode = iota // an unquoted value in text
	valueQuoted                  // a quoted value in text
	valueKeyed                   // a whole JSON value under a key the caller named
)

var (
	passwordWords = set("password", "passwd", "pwd", "pass", "passphrase", "passcode")
	secretWords   = set("secret", "secrets", "token", "credential", "credentials", "auth", "authorization", "cookie",
		"bearer", "apikey", "accesskey", "privatekey", "secretkey", "signingkey", "masterkey", "encryptionkey")
	// secretSuffixes classify a one-word name such as "authtoken" or "dbpassword".
	secretSuffixes   = []string{"token", "secret", "apikey"}
	passwordSuffixes = []string{"password", "passwd"}
	// modifierWords, as a name's last word, make it a name about a secret, not of one:
	// password_policy, password_min_length, token_type, token_endpoint, secret_name.
	modifierWords = set("policy", "policies", "length", "len", "min", "max", "minimum", "maximum",
		"enabled", "enable", "disabled", "required", "require", "reset", "expiry", "expires", "expiration",
		"expired", "strength", "hint", "hints", "rule", "rules", "regex", "pattern", "complexity", "history",
		"age", "lifetime", "ttl", "field", "label", "prompt", "placeholder", "type", "types", "kind", "mode",
		"format", "file", "path", "dir", "env", "var", "variable", "name", "names", "ref", "url", "uri",
		"endpoint", "endpoints", "count", "attempts", "retries", "timeout", "scope", "scopes", "provider",
		"issuer", "audience", "version", "changed", "change", "updated", "created", "at", "date", "time",
		"status", "valid", "validation", "validator")
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// nameTier classifies a name by its words: split at "_", "-" and "." and at case changes
// (apiKey, APIKey), case folded. Two adjacent words count as one too ("api", "key" is apikey).
// Only whole words count: "pass" is a password word, but "passport", "bypass" and "compass"
// are not. "signature" is not a secret word: a model's thinking signature is not the user's
// secret.
//
// A name whose last word is a modifier is about the secret, not the secret itself
// (password_policy, password_min_length): it loses the password tier's no-entropy rule and is
// checked as a secret name, so "strong" or "12" is kept but a generated token still is not.
func nameTier(name string) tier {
	parts := splitName(name)
	t := wordTier(parts)
	if t == tierPassword && len(parts) > 1 && modifierWords[parts[len(parts)-1]] {
		return tierSecret
	}
	return t
}

// wordTier is the highest tier any word of the name, or two adjacent words, gives.
func wordTier(parts []string) tier {
	t := tierNone
	for i, p := range parts {
		words := []string{p}
		if i+1 < len(parts) {
			words = append(words, p+parts[i+1])
		}
		for _, w := range words {
			switch {
			case passwordWords[w] || hasSuffixAny(w, passwordSuffixes):
				return tierPassword
			case secretWords[w] || hasSuffixAny(w, secretSuffixes):
				t = tierSecret
			case w == "key" && t < tierKey:
				t = tierKey
			}
		}
	}
	return t
}

func hasSuffixAny(w string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) {
			return true
		}
	}
	return false
}

// splitName splits a name into lowercase words.
func splitName(name string) []string {
	var parts []string
	start := 0
	flush := func(end int) {
		if end > start {
			parts = append(parts, strings.ToLower(name[start:end]))
		}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || c == '-' || c == '.' {
			flush(i)
			start = i + 1
			continue
		}
		if i > start && c >= 'A' && c <= 'Z' {
			prev := name[i-1]
			nextLower := i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z'
			if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' || (prev >= 'A' && prev <= 'Z' && nextLower) {
				flush(i)
				start = i
			}
		}
	}
	flush(len(name))
	return parts
}

// keywordStart moves back from a name's word at p to the start of the name.
func keywordStart(text string, p int) int {
	s := p
	for s > 0 && p-s < 64 && isNameByte(text[s-1]) {
		s--
	}
	for s < p && !(isLetter(text[s]) || text[s] == '_') {
		s++
	}
	return s
}

func isNameByte(c byte) bool { return isTokenByte(c) || c == '.' }

// findKeywords runs the keyword rule at each trigger position (sorted).
func findKeywords(text string, at []int, out []Edit) []Edit {
	covered, nameRunEnd := 0, -1
	for _, p := range at {
		if p < covered || p < nameRunEnd {
			continue // inside an accepted or long value, or another word of the same name
		}
		// The name's end, and a cheap test for an assignment after it, before the pattern
		// runs: most triggers ("key" in prose, "token" in a type) have none, and a pattern
		// call per trigger would make a text dense with them slow.
		end := p
		for end < len(text) && end-p < 128 && isNameByte(text[end]) {
			end++
		}
		nameRunEnd = end
		if !assignmentAt(text, end) {
			continue
		}
		s := keywordStart(text, p)
		m := matchAt(keywordPattern, text, s, 256)
		if m == nil && quotedValueAt(text, end) {
			// A quoted value whose closing quote is past the first window: look once more,
			// further. Only a quoted value, so a failed trigger costs this once.
			m = matchAt(keywordPattern, text, s, 4096+256)
		}
		if m == nil {
			continue
		}
		g := 2
		for m[2*g] < 0 {
			g++
		}
		vs, ve := s+m[2*g], s+m[2*g+1]
		mode := valueQuoted
		if g == 5 {
			mode = valueBare
		}
		name := text[s+m[2] : s+m[3]]
		if isCookieName(name) {
			// A Cookie header holds several name=value pairs; each value is checked, and the
			// triggers inside the list are not tried again.
			ce := cookieEnd(text, vs)
			out = append(out, cookieEdits(text, vs, ce)...)
			covered = ce
			continue
		}
		t := nameTier(name)
		ok := t != tierNone && secretShaped(t, text[vs:ve], mode) &&
			!(mode == valueBare && (callAfter(text, ve) || t == tierPassword && proseAfter(text, vs, ve)))
		covered = rejectedCover(ok, s+m[1], vs, ve)
		if !ok && ve-vs >= longRejected {
			// A long rejected URL can still hold an assignment in its query string
			// ("auth_url=https://host/...?password=..."): the triggers from its "?" on are tried.
			if q := strings.IndexByte(text[vs:ve], '?'); q >= 0 {
				covered = vs + q
			}
		}
		if ok {
			out = append(out, Edit{Start: vs, End: ve, Cat: t.cat()})
		}
	}
	return out
}

// quotedValueAt reports whether the assignment after a name ending at i opens a quoted value.
func quotedValueAt(text string, i int) bool {
	if i < len(text) && text[i] == '\\' {
		i++
	}
	if i < len(text) && (text[i] == '"' || text[i] == '\'') {
		i++
	}
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == ':' || text[i] == '=' || text[i] == '>') {
		i++
	}
	return i < len(text) && (text[i] == '"' || text[i] == '\'' || text[i] == '\\')
}

// isCookieName reports whether a name is a Cookie or Set-Cookie header's.
func isCookieName(name string) bool {
	n := strings.ToLower(name)
	return n == "cookie" || n == "set-cookie" || n == "cookies"
}

// cookieEnd is where a cookie list starting at i ends: a line break, a quote (raw or escaped)
// or a backslash, at most 4096 bytes on.
func cookieEnd(text string, i int) int {
	end := min(len(text), i+4096)
	if j := strings.IndexAny(text[i:end], "\r\n\"'`\\"); j >= 0 {
		return i + j
	}
	return end
}

// cookieEdits checks each "name=value" pair of a cookie list in text[start:end] ("sid=x;
// session=y") and returns an edit for each value shaped like a secret. A cookie's name and the
// attributes of a Set-Cookie (Path=/, HttpOnly) stay.
func cookieEdits(text string, start, end int) []Edit {
	var out []Edit
	for i := start; i < end; {
		j := end
		if k := strings.IndexByte(text[i:end], ';'); k >= 0 {
			j = i + k
		}
		if eq := strings.IndexByte(text[i:j], '='); eq >= 0 {
			vs := i + eq + 1
			v := strings.TrimSpace(text[vs:j])
			vs += strings.Index(text[vs:j], v)
			if v != "" && !strings.ContainsAny(v, " \t,") && secretShaped(tierSecret, v, valueBare) {
				out = append(out, Edit{Start: vs, End: vs + len(v), Cat: catSecretValue})
			}
		}
		i = j + 1
	}
	return out
}

// proseAfter reports whether a bare value that is one word (letters, perhaps ending in "." or
// ":") is followed by another word, perhaps after a comma: "Error: password: authentication
// failed", API docs' "password: Required. The ..." and "token: Deprecated, use ...". These are
// sentences, not assignments. A password that is one word followed by text is then kept: a known
// limit.
func proseAfter(text string, vs, ve int) bool {
	v := strings.TrimRight(text[vs:ve], ".:")
	if v == "" || strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return false
	}
	i := ve
	if i < len(text) && text[i] == ',' {
		i++
	}
	return i+1 < len(text) && text[i] == ' ' && isLetter(text[i+1])
}

// callAfter reports whether a bare value is followed by "(", "<" or "[": a call, a generic or
// an index ("hash_password(raw)", "useRef<HTMLInputElement>"), which is code, not a secret.
func callAfter(text string, ve int) bool {
	return ve < len(text) && (text[ve] == '(' || text[ve] == '<' || text[ve] == '[')
}

// assignmentAt reports whether an assignment follows a name ending at i: an optional closing
// quote (straight or escaped), spaces or tabs, then ":" or "=".
func assignmentAt(text string, i int) bool {
	if i < len(text) && text[i] == '\\' {
		i++
	}
	if i < len(text) && (text[i] == '"' || text[i] == '\'') {
		i++
	}
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i < len(text) && (text[i] == ':' || text[i] == '=')
}

// keyedValue checks a whole text value under a JSON key. An Authorization-style value
// ("Bearer x", "Basic x") is checked without its scheme, which stays. A password is checked
// whole, whatever it starts with: "token My passphrase!" is a password, not a scheme.
func keyedValue(text, key string) (Edit, bool) {
	t := nameTier(key)
	if t == tierNone {
		return Edit{}, false
	}
	start := len(text) - len(strings.TrimLeft(text, " \t"))
	v := strings.TrimRight(text[start:], " \t")
	for _, scheme := range []string{"bearer ", "basic ", "token ", "digest ", "negotiate ", "ntlm "} {
		if t != tierPassword && len(v) > len(scheme) && strings.EqualFold(v[:len(scheme)], scheme) {
			// An explicit credential scheme: the token after it is the secret, whatever its
			// entropy, if it is one token.
			tok := strings.TrimLeft(v[len(scheme):], " ")
			ts := start + len(v) - len(tok)
			if len(tok) >= 8 && !strings.ContainsAny(tok, " \t\r\n") {
				return Edit{Start: ts, End: ts + len(tok), Cat: catCredential}, true
			}
			return Edit{}, false
		}
	}
	if !secretShaped(t, v, valueKeyed) {
		return Edit{}, false
	}
	return Edit{Start: start, End: start + len(v), Cat: t.cat()}, true
}

// secretShaped reports whether v, assigned to a name of tier t, is a secret.
func secretShaped(t tier, v string, mode valueMode) bool {
	if len(v) > 4096 || strings.ContainsAny(v, "\r\n") || notASecret(v) {
		return false
	}
	if uuidPattern.MatchString(v) {
		return t != tierKey // a UUID api key is a secret; an idempotency key is an id
	}
	if t == tierPassword {
		if len(v) < 6 || mode == valueBare && typeNamePattern.MatchString(v) {
			return false
		}
		switch {
		case !strings.ContainsAny(v, " \t"):
			return true
		case mode == valueKeyed:
			return true
		default: // a quoted value with spaces: a passphrase if it has more than words in it
			return strings.Trim(v, " \tabcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") != ""
		}
	}
	if t == tierKey && (len(v) < 20 || strings.ContainsAny(v, ":.") || isHex(v)) {
		return false
	}
	return tokenShaped(v)
}

var (
	uuidPattern      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	knownIDPattern   = regexp.MustCompile(`^(?:toolu_bdrk_|toolu_|srvtoolu_|msg_bdrk_|msg_|req_)[A-Za-z0-9]{20,}$`)
	dottedRefPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+(?:\(\))?$`)
	// typeNamePattern is a type name of two or more capitalized words, such as
	// "HTMLInputElement" or "PasswordField": what a password-named field is declared as.
	typeNamePattern = regexp.MustCompile(`^[A-Z]+[a-z]+(?:[A-Z]+[a-z]*)+$`)
	placeholders    = set("changeme", "password", "passwd", "secret", "none", "null", "nil", "true", "false",
		"undefined", "string", "str", "bytes", "number", "boolean", "bool", "int", "text", "char", "required",
		"optional", "hidden", "masked", "dummy", "placeholder", "redacted", "unset", "empty", "default", "prompt")
)

// notASecret reports whether v is a placeholder, a reference, a path, a URL or a known id: a
// value a secret name is often given that is not itself a secret.
func notASecret(v string) bool {
	if v == "" || strings.Count(v, v[:1]) == len(v) { // "xxxxxxxx", "********"
		return true
	}
	lower := strings.ToLower(v)
	switch {
	case placeholders[lower],
		strings.ContainsAny(v[:1], "$%<[{~"), // ${VAR}, $VAR, %VAR%, <key>, [REDACTED:...], {{x}}, ~/x
		v[0] == '/' && wordLike(v),           // /home/user/project; a base64 token may start with "/" too
		strings.HasPrefix(v, "./"), strings.HasPrefix(v, "../"),
		len(v) > 2 && v[1] == ':' && (v[2] == '\\' || v[2] == '/'), // C:\x, C:/x
		strings.Contains(v, "://"),
		strings.Contains(lower, "redacted"), strings.Contains(lower, "xxxx"), strings.Contains(v, "***"),
		// "your-api-key-here", "your_password": words only. "yourH0rse!2026" is a password.
		strings.HasPrefix(lower, "your") && strings.Trim(lower, "abcdefghijklmnopqrstuvwxyz-_ ") == "",
		strings.Contains(lower, "example"), strings.Contains(lower, "placeholder"):
		return true
	}
	return knownIDPattern.MatchString(v) || dottedRefPattern.MatchString(v) || envNamePattern.MatchString(v)
}

// envNamePattern is an environment variable's name, in capitals with an underscore
// ("password_env: DB_PASSWORD"): the name of where the secret is, not the secret.
var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$`)

// isHex reports whether v is hex digits only, in one case.
func isHex(v string) bool {
	return strings.Trim(v, "0123456789abcdef") == "" || strings.Trim(v, "0123456789ABCDEF") == ""
}

// tokenShaped reports whether v looks like a generated token rather than words or an
// identifier: 16 or more characters, no whitespace, not word-like, two kinds of character (or
// 32 or more of one), and entropy near what a random string of its alphabet and length has.
func tokenShaped(v string) bool {
	if len(v) < 16 || strings.ContainsAny(v, " \t") || wordLike(v) {
		return false
	}
	if !looksLikeToken(v) && (len(v) < 32 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "") {
		return false
	}
	return entropy(v) >= entropyFloor(alphabetSize(v), len(v))
}

// entropyFloor is the least entropy a random string of n characters from an alphabet of k has,
// but for about one in a thousand: the expected plug-in entropy of such a string, less a margin
// for its spread. The expectation is exact (see expectedEntropy); the margin was fitted so that
// fewer than 1% of random tokens fall below it, for every alphabet and length the tests try.
func entropyFloor(k, n int) float64 {
	return expectedEntropy(k, n) - entropyMargin(k, n)
}

// entropyMargin is the spread allowed below the expected entropy: about three standard
// deviations of a random string's plug-in entropy, which shrinks as the string grows.
func entropyMargin(k, n int) float64 {
	return 0.15 + 2.6*math.Sqrt(float64(min(k, n)))/float64(n)
}

// expectedEntropy is the expected Shannon entropy, in bits, of the character frequencies of n
// characters drawn uniformly from k. By linearity it is k times the expected contribution of
// one character, whose count X is Binomial(n, 1/k): E[-(X/n) log2(X/n)].
func expectedEntropy(k, n int) float64 {
	p := 1 / float64(k)
	px := math.Pow(1-p, float64(n)) // P(X = 0)
	e := 0.0
	for x := 1; x <= n; x++ {
		px *= float64(n-x+1) / float64(x) * p / (1 - p) // P(X = x) from P(X = x-1)
		if px < 1e-12 && float64(x) > float64(n)*p {
			break // the rest of the tail adds nothing
		}
		f := float64(x) / float64(n)
		e -= px * f * math.Log2(f)
	}
	return float64(k) * e
}

// alphabetSize is the size of the alphabet v looks drawn from: 16 for hex, else 26 per letter
// case, 10 for digits, and for the other characters the base64 two ("+/", "-_") or, if there
// are more kinds, as many as appear, at least 2. A trailing "=" (base64 padding) is not counted.
func alphabetSize(v string) int {
	v = strings.TrimRight(v, "=")
	if isHex(v) {
		return 16
	}
	var lower, upper, digit bool
	var others [256]bool
	nOther := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		default:
			if !others[c] {
				others[c] = true
				nOther++
			}
		}
	}
	n := 0
	if lower {
		n += 26
	}
	if upper {
		n += 26
	}
	if digit {
		n += 10
	}
	if nOther > 0 {
		n += max(2, nOther)
	}
	return max(n, 2)
}

// wordLike reports whether v is made of words, as an identifier is ("my-k8s-secret-name",
// "getUserProfile", "user_profile_settings_id"): two or more chunks, split at "- _ . / :" and
// at case changes, each a word (two or more letters, lowercase, uppercase or capitalized), a
// number of up to 4 digits, or a short lowercase-and-digit piece ("k8s", "v2"); and words of
// three or more letters hold most of its letters. A random token splits into short mixed chunks
// and is not word-like.
//
// Only words that read as words count toward "most": four or more letters, or two or three
// with a vowel ("get", "key", "my", "IO"; a two-letter one in one case). Random letters make
// many short chunks without vowels, so their short chunks do not add up to a word-like token.
func wordLike(v string) bool {
	// In camelCase a capital starts each word: a fifth of the letters or so, more with an
	// acronym. Random mixed case is half capitals.
	// English words use the rare letters (b f g j k p q v w x y z) for about a seventh of their
	// letters; random letters for nearly half.
	upper, lower, rare := 0, 0, 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z':
			upper++
		case c >= 'a' && c <= 'z':
			lower++
		}
		if isLetter(c) && strings.IndexByte("bfgjkpqvwxyz", lowerByte[c]) >= 0 {
			rare++
		}
	}
	if upper > 0 && lower > 0 && upper*10 > (upper+lower)*4 || rare*10 > (upper+lower)*4 {
		return false
	}
	chunks, chars, inWords := 0, 0, 0
	parts := splitChunks(v)
	for i, c := range parts {
		chunks++
		chars += len(c)
		// A word may end in a short number ("Total1", "Database2024").
		w := strings.TrimRight(c, "0123456789")
		if len(c)-len(w) > 4 {
			w = c
		}
		nl := 0
		for j := 0; j < len(c); j++ {
			if isLetter(c[j]) {
				nl++
			}
		}
		vowel := strings.ContainsAny(w, "aeiouyAEIOUY")
		switch {
		case len(c) == 1 && (c[0] >= 'A' && c[0] <= 'Z' && i+1 < len(parts) || i == 0 && isLetter(c[0])):
			// the "O" of OAuth, the "X" of getXCoordinate, the "i" of iOS; not a capital at
			// the end, which random mixed case often has
		case nl == len(w) && len(w) >= 2 && isWordCase(w) && pronounceable(w):
			if len(w) >= 4 || vowel && (len(w) == 3 || strings.ToLower(w) == w || strings.ToUpper(w) == w) {
				inWords += nl
			}
		case nl == 0 && len(c) <= 4:
		case len(c) >= 2 && len(c) <= 3 && strings.ContainsAny(c, "0123456789"): // k8s, v2, X86, K8s
		default:
			return false // a lone lowercase letter, a long mixed piece, or letters with no vowel
		}
	}
	// Words are long: an identifier's chunks average 4 characters or more, random case changes
	// cut a token into chunks of 2 or 3.
	return chunks >= 2 && inWords*10 >= chars*7 && chars >= chunks*3
}

// vowellessWords are words identifiers hold that have no vowel.
var vowellessWords = set("http", "https", "html", "xhtml", "grpc", "sftp", "smtp", "pkcs", "nvme",
	"db", "js", "ts", "md", "pr", "fs", "vm", "cv", "dr", "mr", "ms", "pm", "rx", "tx", "px", "sh", "pg")

// pronounceable reports whether a word reads as a word: a two-letter one, or one of four or more,
// has a vowel ("y" counts), and one of five or more has at least 15% vowels and no run of five
// consonants; or it is a known vowelless word (HTTP, db). Random letters often fail: a run of
// four has no vowel about a third of the time, and long runs have long consonant runs.
func pronounceable(w string) bool {
	lower := strings.ToLower(w)
	if len(w) == 3 || vowellessWords[lower] {
		return true
	}
	vowels, run := 0, 0
	for i := 0; i < len(lower); i++ {
		if strings.IndexByte("aeiouy", lower[i]) >= 0 {
			vowels++
			run = 0
		} else if run++; run >= 5 {
			return false
		}
	}
	return vowels > 0 && (len(w) < 5 || vowels*100 >= len(w)*15)
}

// isWordCase reports whether a chunk of letters is lowercase, uppercase or capitalized.
func isWordCase(c string) bool {
	lower := strings.ToLower(c)
	return c == lower || c == strings.ToUpper(c) || c[1:] == lower[1:]
}

// splitChunks splits v at separators and at case changes (a lowercase letter before an
// uppercase one; the last capital of a run before a lowercase letter).
func splitChunks(v string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || strings.IndexByte("-_./:", v[i]) >= 0 {
			if i > start {
				out = append(out, v[start:i])
			}
			start = i + 1
			continue
		}
		if i > start && v[i] >= 'A' && v[i] <= 'Z' {
			prev := v[i-1]
			nextLower := i+1 < len(v) && v[i+1] >= 'a' && v[i+1] <= 'z'
			// After a lowercase letter or a digit ("OAuth2Client"), or the last capital of a
			// run before a lowercase letter.
			if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' || (prev >= 'A' && prev <= 'Z' && nextLower) {
				out = append(out, v[start:i])
				start = i
			}
		}
	}
	return out
}

// entropy is v's Shannon entropy in bits per byte.
func entropy(v string) float64 {
	if v == "" {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(v); i++ {
		freq[v[i]]++
	}
	h, n := 0.0, float64(len(v))
	for _, c := range freq {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
