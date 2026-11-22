package content

import (
	"regexp"
	"strings"
)

// The categories, each a marker's name.
const (
	catAPIKey      = "api_key"
	catPrivateKey  = "private_key"
	catJWT         = "jwt"
	catPassword    = "password"
	catCredential  = "credential"
	catSecretValue = "secret_value"
	catEncoded     = "encoded_secret"
)

// THE SOURCE NEVER HOLDS A COMPLETE SECRET-SHAPED STRING. Secret scanners run on every commit,
// so the key-block headers spell their spaces as "[ ]", and no pattern is a literal token.

// anchored is a rule whose pattern is tried at each of its triggers, anchored there.
type anchored struct {
	cat      string
	triggers []string // lowercase; the pattern is tried where one starts
	re       *regexp.Regexp
	group    int // the submatch that is replaced; 0 is the whole match
	span     int // how far a match can reach from its start before the window grows

	// start moves a trigger's position back to where a match would start (a URL's scheme
	// before its "://"); nil means the match starts at the trigger.
	start func(text string, p int) int
	// before requires a boundary (a byte that cannot continue a token, or the start of the
	// text) just before the match; after requires one just after the replaced span.
	before, after bool
	// check, if set, must accept the replaced value.
	check func(v string) bool
	// anyGroup replaces the first submatch that took part, for a pattern whose value is one
	// of several alternatives (quoted or bare); group is then ignored.
	anyGroup bool
	// segment marks a pattern that scans ahead to the end of a command (a line break, "|",
	// ";" or "&") and no further. After a failed match, the triggers up to there are skipped:
	// they would scan the same command again.
	segment bool
}

// segmentEnd is where a segment rule's scan from s ended: the end of the command, or of the
// window it was matched in.
func segmentEnd(text string, s, span int) int {
	w := min(len(text), s+span)
	if i := strings.IndexAny(text[s:w], "\n|;&"); i >= 0 {
		return s + i
	}
	return w
}

// apiKeys are provider tokens with a fixed prefix. Each starts with its trigger. A fixed-length
// one needs a boundary after it too, so a longer run of the same characters is not a key.
var apiKeys = []*anchored{
	key([]string{"sk-"}, `sk-(?:ant-[A-Za-z0-9_-]{20,}|(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|[A-Za-z0-9]{20,})`, false), // Anthropic, OpenAI
	key([]string{"akia", "asia", "abia", "acca"}, `(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}`, true),                              // AWS access key ids
	key([]string{"aiza"}, `AIza[A-Za-z0-9_-]{35}`, true),                                                                    // Google Cloud
	key([]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, `gh[pousr]_[A-Za-z0-9]{36,}`, false),                              // GitHub tokens
	key([]string{"github_pat_"}, `github_pat_[A-Za-z0-9_]{22,}`, false),                                                     // GitHub fine-grained tokens
	key([]string{"glpat-", "gldt-"}, `gl(?:pat|dt)-[A-Za-z0-9_-]{20,}`, false),                                              // GitLab personal and deploy tokens
	key([]string{"hf_"}, `hf_[A-Za-z0-9]{34,}`, false),                                                                      // Hugging Face
	key([]string{"xox"}, `xox[abeprs][.-][A-Za-z0-9.-]{10,}`, false),                                                        // Slack tokens, incl. refresh (xoxe)
	key([]string{"xapp-"}, `xapp-[A-Za-z0-9-]{10,}`, false),                                                                 // Slack app tokens
	key([]string{"sk_live_", "sk_test_", "rk_live_", "rk_test_"}, `[sr]k_(?:live|test)_[A-Za-z0-9]{20,}`, false),            // Stripe secret and restricted keys
	key([]string{"npm_"}, `npm_[A-Za-z0-9]{20,}`, false),                                                                    // npm
	key([]string{"pypi-"}, `pypi-[A-Za-z0-9_-]{20,}`, false),                                                                // PyPI
	key([]string{"sg."}, `SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}`, true),                                                  // SendGrid
	key([]string{"dapi"}, `dapi[a-f0-9]{32}(?:-[0-9])?`, true),                                                              // Databricks
	{
		cat: catAPIKey, triggers: []string{"hooks.slack.com/services/"}, span: 256, after: true,
		re:    regexp.MustCompile(`^(?:https?://)?hooks\.slack\.com/services/T[A-Za-z0-9_]+/B[A-Za-z0-9_]+/[A-Za-z0-9_]+`),
		start: func(text string, p int) int { return p - len(prefixBefore(text, p, "https://", "http://")) },
	}, // Slack webhooks, replaced with their scheme
}

func key(triggers []string, pattern string, fixedLength bool) *anchored {
	return &anchored{cat: catAPIKey, triggers: triggers, re: regexp.MustCompile(`^` + pattern), span: 512, before: true, after: fixedLength}
}

// jwt is three base64url segments, the first two starting with "eyJ".
var jwt = &anchored{
	cat: catJWT, triggers: []string{"eyj"}, span: 512, before: true,
	re: regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// tokenClass is the characters of an HTTP credential token: base64 and base64url, with "." and
// "~". It stops at a quote, a backslash, whitespace and JSON punctuation.
const tokenClass = `[A-Za-z0-9._~+/=-]`

// authSchemes are the Authorization schemes whose token is replaced and whose name stays.
const authSchemes = `bearer|basic|token|digest|negotiate|ntlm|aws4-hmac-sha256`

// authorization is an Authorization header or pair, in text or JSON ("Authorization: Bearer
// x", `"authorization":"Basic x"`, `\"Authorization\": \"token x\"`). Group 2 is the token; the
// scheme stays. A value with no scheme must look like a token (16+ characters, two kinds of
// character), so "authorization: required" is kept.
var authorization = &anchored{
	cat: catCredential, triggers: []string{"authorization"}, group: 2, span: 512,
	re: regexp.MustCompile(`(?i)^authorization\\?["']?[ \t]*[:=][ \t]*\\?["']?[ \t]*` +
		`((?:` + authSchemes + `)[ \t]+)?(` + tokenClass + `{8,})`),
}

// sigV4 is an AWS Signature Version 4 signature, in an Authorization header or a presigned URL
// ("Signature=<64 hex>", "X-Amz-Signature=<64 hex>"): a credential until it expires.
var sigV4 = &anchored{
	cat: catCredential, triggers: []string{"signature="}, group: 1, span: 128, after: true,
	re: regexp.MustCompile(`(?i)^signature=([0-9a-f]{64})`),
}

// djangoSecretKey is a Django SECRET_KEY: 50 characters from an alphabet that holds "( ) & ="
// and other punctuation the keyword rule stops at, sometimes after "django-insecure-". The
// value must be quoted, as in settings.py.
var djangoSecretKey = &anchored{
	cat: catSecretValue, triggers: []string{"secret_key"}, group: 1, span: 256,
	re: regexp.MustCompile(`(?i)^secret_key[ \t]*=[ \t]*["']((?:django-insecure-)?[a-z0-9!@#$%^&*(\-_=+)]{40,60})["']`),
	check: func(v string) bool {
		v = strings.ToLower(strings.TrimPrefix(v, "django-insecure-"))
		return strings.Count(v, v[:1]) != len(v) && !strings.Contains(v, "xxxx") && !strings.Contains(v, "change") &&
			!strings.Contains(v, "example") && !strings.HasPrefix(v, "your")
	},
}

// bearer is a bearer token anywhere ("use Bearer x"). It must look like a token, so prose such as
// "the bearer of_this_message_here" is kept.
var bearer = &anchored{
	cat: catCredential, triggers: []string{"bearer"}, group: 1, span: 512, before: true,
	re:    regexp.MustCompile(`(?i)^bearer[ \t]+(` + tokenClass + `{20,})`),
	check: looksLikeToken,
}

// anchoredRules are the pattern rules found by trigger, beside the key blocks, URL passwords, the
// keyword rule and the encoded rule, which have their own finders.
var anchoredRules = append(append(append([]*anchored{}, apiKeys...), jwt, authorization, bearer, sigV4, djangoSecretKey), cliRules...)

// longRejected is the length from which a rejected candidate's triggers are skipped. A shorter
// one is not: a trigger inside it can start a secret of its own
// ("auth_url=https://x.io/login?password=..."). A longer one is skipped whole, so a text that
// repeats a trigger inside one long run stays linear.
const longRejected = 256

// find runs the rule at each trigger position (sorted), adding its edits to out.
func (a *anchored) find(text string, at []int, out []Edit) []Edit {
	covered, lastStart := 0, -1
	for _, p := range at {
		if p < covered {
			continue // inside an accepted match, or a long rejected one
		}
		s := p
		if a.start != nil {
			s = a.start(text, p)
		}
		if s == lastStart {
			continue
		}
		lastStart = s
		// The boundary is checked before the pattern runs: a trigger inside a longer token
		// (every "eyJ" of "eyJeyJeyJ...") is then rejected at once, not after a scan of the
		// whole token, which would make the rule quadratic.
		if a.before && !boundaryBefore(text, s) {
			continue
		}
		m := matchAt(a.re, text, s, a.span)
		if m == nil {
			if a.segment {
				covered = segmentEnd(text, s, a.span)
			}
			continue
		}
		g := a.group
		if a.anyGroup {
			for g = 1; m[2*g] < 0; g++ {
			}
		}
		vs, ve := s+m[2*g], s+m[2*g+1]
		ok := !(a == authorization && m[2] < 0 && !(ve-vs >= 16 && looksLikeToken(text[vs:ve]))) &&
			!(a.after && !boundaryAfter(text, ve)) &&
			(a.check == nil || a.check(text[vs:ve]))
		covered = rejectedCover(ok, s+m[1], vs, ve)
		if ok {
			out = append(out, Edit{Start: vs, End: ve, Cat: a.cat})
		}
	}
	return out
}

// rejectedCover is where the next trigger may start after a candidate: past the match when it
// was accepted or is long, else at its value, so a trigger inside a short rejected value is
// still tried.
func rejectedCover(accepted bool, end, vs, ve int) int {
	if accepted || ve-vs >= longRejected {
		return end
	}
	return vs
}

// matchAt matches the anchored re at text[s:], first within span bytes; when the match reaches
// the window's end, it may continue past it, so it is matched again on the rest of the text.
func matchAt(re *regexp.Regexp, text string, s, span int) []int {
	w := min(len(text), s+span)
	m := re.FindStringSubmatchIndex(text[s:w])
	if m != nil && s+m[1] == w && w < len(text) {
		m = re.FindStringSubmatchIndex(text[s:])
	}
	return m
}

// isTokenByte reports whether c can continue a token: a letter, a digit, "_" or "-".
func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// boundaryBefore reports whether a token can start at i: at the start of the text, or after a
// byte that cannot continue a token. In raw JSON text, the escapes \n \r \t before it count as
// a boundary too (the "n" of "\n" is not a letter of the token), unless the backslash is
// itself escaped.
func boundaryBefore(text string, i int) bool {
	if i == 0 || !isTokenByte(text[i-1]) {
		return true
	}
	if c := text[i-1]; c == 'n' || c == 'r' || c == 't' {
		n := 0
		for j := i - 2; j >= 0 && text[j] == '\\'; j-- {
			n++
		}
		return n%2 == 1
	}
	return false
}

// boundaryAfter reports whether a token can end at i.
func boundaryAfter(text string, i int) bool { return i == len(text) || !isTokenByte(text[i]) }

// schemeStart moves back from a "://" at p to the start of its scheme: up to 32 scheme
// characters, starting with a letter.
func schemeStart(text string, p int) int {
	s := p
	for s > 0 && p-s < 32 && isSchemeByte(text[s-1]) {
		s--
	}
	for s < p && !isLetter(text[s]) {
		s++
	}
	return s
}

func isSchemeByte(c byte) bool {
	return isLetter(c) || c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-'
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// prefixBefore returns the first of prefixes that text holds just before p, or "".
func prefixBefore(text string, p int, prefixes ...string) string {
	for _, pre := range prefixes {
		if strings.HasSuffix(text[:p], pre) {
			return pre
		}
	}
	return ""
}

// looksLikeToken reports whether v has at least two kinds of character among lowercase,
// uppercase and digits, as a generated token does and a word or an identifier in one case
// does not.
func looksLikeToken(v string) bool {
	var lower, upper, digit bool
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	n := 0
	for _, b := range []bool{lower, upper, digit} {
		if b {
			n++
		}
	}
	return n >= 2
}
