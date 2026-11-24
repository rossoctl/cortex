package content

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// NO COMPLETE SECRET-SHAPED STRING APPEARS IN THESE TESTS. Secret scanners run on every commit,
// and an inline "ignore" marker does not stop them, so each test secret is assembled at runtime
// from pieces (a prefix split in two, then filler), with obviously fake values: repeated
// letters, or a patterned run of characters where a rule needs high entropy.

// fill is n copies of c.
func fill(c string, n int) string { return strings.Repeat(c, n) }

// mixed is n characters cycling lowercase, uppercase and digits in a fixed pattern: high entropy
// and several kinds of character, as a generated token has, but plainly not a real one.
func mixed(n int) string {
	const lower, upper, digit = "abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "0123456789"
	b := make([]byte, n)
	for i := range b {
		switch i % 3 {
		case 0:
			b[i] = lower[(i*7)%26]
		case 1:
			b[i] = upper[(i*11)%26]
		default:
			b[i] = digit[i%10]
		}
	}
	return string(b)
}

// hexRun is n lowercase hex digits cycling through all sixteen.
func hexRun(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[(i*7)%16]
	}
	return string(b)
}

// fakePassword is a short human-style password: no entropy to speak of.
func fakePassword() string { return "hunter" + "22" }

// fakes builds one secret-shaped value per provider rule, from pieces.
func fakes() map[string]string {
	return map[string]string{
		"anthropic":      "sk-" + "ant-" + "api03-" + fill("A", 26) + "-" + fill("B", 19),
		"openai-proj":    "sk-" + "proj-" + fill("C", 24),
		"openai-svcacct": "sk-" + "svcacct-" + fill("C", 24),
		"openai-admin":   "sk-" + "admin-" + fill("C", 24),
		"openai":         "sk-" + fill("D", 24),
		"aws":            "AK" + "IA" + fill("E", 16),
		"aws-temporary":  "AS" + "IA" + fill("E", 16),
		"gcp":            "AI" + "za" + fill("F", 35),
		"github":         "gh" + "p_" + fill("G", 36),
		"github-oauth":   "gh" + "o_" + fill("G", 36),
		"github-pat":     "github" + "_pat_" + fill("H", 22),
		"gitlab":         "gl" + "pat-" + fill("I", 20),
		"gitlab-dt":      "gl" + "dt-" + fill("J", 20),
		"hf":             "h" + "f_" + fill("K", 34),
		"slack":          "xo" + "xb-" + fill("L", 12),
		"slack-refresh":  "xo" + "xe-" + fill("L", 12),
		"slack-app":      "xa" + "pp-" + fill("L", 12),
		"stripe":         "sk" + "_live_" + fill("M", 20),
		"stripe-test":    "sk" + "_test_" + fill("M", 20),
		"stripe-rk":      "rk" + "_live_" + fill("M", 20),
		"npm":            "np" + "m_" + fill("N", 20),
		"pypi":           "py" + "pi-" + fill("O", 20),
		"sendgrid":       "S" + "G." + fill("P", 22) + "." + fill("Q", 43),
		"databricks":     "da" + "pi" + hexRun(32),
		"slack-hook":     "https://hooks." + "slack.com/services/" + "T" + fill("S", 8) + "/B" + fill("T", 8) + "/" + fill("U", 12),
	}
}

// pemBlock is a fake private key block of the given kind ("RSA", "EC", ""), assembled so no
// header appears in the source.
func pemBlock(kind string) string {
	k := "PRIVATE" + " KEY"
	if kind != "" {
		k = kind + " " + k
	}
	return "-----" + "BEGIN " + k + "-----\n" + fill("M", 40) + "\n" + fill("N", 20) + "==\n-----" + "END " + k + "-----"
}

// fakeJWT is three base64url segments, the first two starting with "eyJ".
func fakeJWT() string {
	return "ey" + "J" + fill("a", 16) + ".ey" + "J" + fill("b", 16) + "." + fill("c", 20)
}

func redact(s string) (string, Stats) { return New().String(s) }

// mustBeJSON fails the test when s is not valid JSON.
func mustBeJSON(t *testing.T, what, s string) {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Errorf("%s is not valid JSON: %q", what, s)
	}
}

// Find returns byte offsets into the text it was given, sorted, and Apply puts the markers there;
// text before a secret that is not ASCII does not shift them.
func TestFindAndApply(t *testing.T) {
	k1, k2 := fakes()["github"], fakes()["openai"]
	text := "שלום " + k1 + " and " + k2 + "."
	edits := New().Find(text, Context{})
	if len(edits) != 2 {
		t.Fatalf("edits = %v, want 2", edits)
	}
	if got := text[edits[0].Start:edits[0].End]; got != k1 {
		t.Errorf("edit 0 covers %q, want the first key", got)
	}
	if got := text[edits[1].Start:edits[1].End]; got != k2 {
		t.Errorf("edit 1 covers %q, want the second key", got)
	}
	if out := Apply(text, edits); out != "שלום [REDACTED:api_key] and [REDACTED:api_key]." {
		t.Errorf("Apply = %q", out)
	}
	if st := Count(edits); st["api_key"] != 2 || len(st) != 1 {
		t.Errorf("Count = %v", st)
	}
}

// Overlapping finds become one edit, named for the higher category: a JWT in an Authorization
// header is one redaction, not a credential with a JWT inside it.
func TestOverlappingEditsMerge(t *testing.T) {
	text := "Authorization: Bearer " + fakeJWT()
	edits := New().Find(text, Context{})
	if len(edits) != 1 || edits[0].Cat != "jwt" {
		t.Fatalf("edits = %v, want one jwt edit", edits)
	}
	if out := Apply(text, edits); out != "Authorization: Bearer [REDACTED:jwt]" {
		t.Errorf("out = %q", out)
	}
}

// Short text is returned as is, and text with nothing in it is returned unchanged.
func TestNothingToRedact(t *testing.T) {
	for _, in := range []string{"", "short", "a plain sentence with no secret in it at all"} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q: out %q, stats %v", in, out, st)
		}
		if e := New().Find(in, Context{}); e != nil {
			t.Errorf("%q: Find = %v, want nil", in, e)
		}
	}
}

// Bytes returns the input slice itself when nothing matched, so an unchanged body is not copied.
func TestBytes_UnchangedIsTheSameSlice(t *testing.T) {
	b := []byte(`{"model":"some-model","messages":[{"role":"user","content":"hello there"}]}`)
	out, st := New().Bytes(b)
	if &out[0] != &b[0] || len(st) != 0 {
		t.Fatalf("Bytes copied or changed an unchanged body: %q, stats %v", out, st)
	}
}

// Redacting twice changes nothing more: a marker is not itself a secret, nor a keyword's value.
func TestIdempotent(t *testing.T) {
	in := "k=" + fakes()["github"] + " " + pemBlock("EC") + " " + fakeJWT() + " password=" + fakePassword() +
		` {"client_secret":"` + mixed(24) + `"} postgres://u:` + fill("p", 9) + "@db/x"
	once, st1 := redact(in)
	if len(st1) < 5 {
		t.Fatalf("the first pass found too little: %v in %q", st1, once)
	}
	twice, st := redact(once)
	if twice != once || len(st) != 0 {
		t.Errorf("a second pass changed %q to %q (stats %v)", once, twice, st)
	}
}

// THE PLANTED-SECRET TEST. One secret of each category is planted in a request body, an SSE
// stream and an error message, as raw bytes: the shapes a body has on the wire, redacted whole
// as text (the fallback for a body that is not parsed). The redacted bytes must hold none of
// them, every category must be counted, and every JSON document must still be valid JSON, with
// the stream framing intact.
func TestPlantedSecretsLeaveNoTrace(t *testing.T) {
	f := fakes()
	planted := map[string]string{
		"api_key":        f["openai"],
		"private_key":    pemBlock("RSA"),
		"jwt":            fakeJWT(),
		"password":       fill("v", 14), // the password of the URL below
		"credential":     fill("W", 28), // the bearer value below
		"secret_value":   mixed(26),
		"encoded_secret": base64.StdEncoding.EncodeToString([]byte(f["anthropic"])), // 64+ characters encoded
	}
	if len(planted["encoded_secret"]) < 64 {
		t.Fatalf("the encoded fixture is %d characters, under the rule's 64", len(planted["encoded_secret"]))
	}
	pemJSON := strings.ReplaceAll(planted["private_key"], "\n", `\n`) // as it sits inside a JSON string
	request := `{"model":"m","messages":[{"role":"user","content":"my key is ` + planted["api_key"] +
		` and the db is postgres://app:` + planted["password"] + `@db.example.com/x"}],` +
		`"metadata":{"note":"client_secret=` + planted["secret_value"] + `","blob":"` + planted["encoded_secret"] + `"}}`
	data1 := `{"delta":{"text":"` + pemJSON + `"}}`
	data2 := `{"delta":{"text":"token ` + planted["jwt"] + `"}}`
	errMsg := `{"error":{"message":"rejected","headers":{"Authorization":"Bearer ` + planted["credential"] + `"}}}`
	for name, doc := range map[string]string{"request": request, "data 1": data1, "data 2": data2, "error": errMsg} {
		mustBeJSON(t, "the fixture "+name, doc)
	}
	sse := "event: content_block_delta\ndata: " + data1 + "\n\ndata: " + data2 + "\n\n"
	all := request + "\n" + sse + errMsg

	b, st := New().Bytes([]byte(all))
	out := string(b)
	for cat, secret := range planted {
		if strings.Contains(out, secret) {
			t.Errorf("%s survived in the output", cat)
		}
		if st[cat] == 0 {
			t.Errorf("no %s was counted (stats %v)", cat, st)
		}
	}
	// The key block sat in the stream with its line breaks escaped: none of it, in that form,
	// survives either, so a redacted header alone would not pass.
	if strings.Contains(out, pemJSON) || strings.Contains(out, fill("M", 40)) {
		t.Error("the key block's body survived in its on-wire form")
	}
	for _, sep := range []string{`"`, "data: ", "\n\n", "event: "} {
		if strings.Count(out, sep) != strings.Count(all, sep) {
			t.Errorf("the count of %q changed: %d -> %d", sep, strings.Count(all, sep), strings.Count(out, sep))
		}
	}
	// Every JSON document is still JSON, found by its framing: the request line, the two data
	// lines, the error line.
	lines := strings.Split(out, "\n")
	var docs []string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			docs = append(docs, rest)
		}
	}
	docs = append([]string{lines[0]}, append(docs, lines[len(lines)-1])...)
	if len(docs) != 4 {
		t.Fatalf("found %d documents in the redacted output, want 4: %q", len(docs), out)
	}
	for i, doc := range docs {
		mustBeJSON(t, []string{"the request", "data 1", "data 2", "the error"}[i], doc)
	}
}
