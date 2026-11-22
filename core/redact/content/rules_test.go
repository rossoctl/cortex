package content

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Each provider key is replaced whole, in any surrounding text, and counted as api_key.
func TestAPIKeys(t *testing.T) {
	for name, key := range fakes() {
		t.Run(name, func(t *testing.T) {
			for _, wrap := range [][2]string{{"before ", " after"}, {`{"k":"`, `"}`}, {"(", ")"}, {"", ""}} {
				in := wrap[0] + key + wrap[1]
				out, st := redact(in)
				if want := wrap[0] + "[REDACTED:api_key]" + wrap[1]; out != want {
					t.Errorf("%q: out = %q, want %q", in, out, want)
				}
				if st["api_key"] != 1 {
					t.Errorf("%q: stats = %v, want one api_key", in, st)
				}
			}
		})
	}
}

// A provider prefix with too little after it is not a key.
func TestAPIKeys_TooShortIsKept(t *testing.T) {
	for _, s := range []string{"sk-" + fill("A", 10), "gh" + "p_" + fill("G", 20), "AK" + "IA" + fill("E", 8), "da" + "pi" + hexRun(20)} {
		if out, st := redact("value " + s + " end"); out != "value "+s+" end" || len(st) != 0 {
			t.Errorf("%q was redacted: %q", s, out)
		}
	}
}

// A provider prefix inside a longer token is not a key: "task-" holds "sk-", and a fixed-length
// key followed by more of its characters is something else.
func TestAPIKeys_NeedABoundary(t *testing.T) {
	for _, s := range []string{
		"task-" + "a1b2c3d4e5f6a7b8c9d0e1f2a3b4",     // a task id holding "sk-"
		"mask-" + fill("x", 30),                      // a name holding "sk-"
		"xAK" + "IA" + fill("E", 16),                 // AKIA inside a word
		"AK" + "IA" + fill("E", 16) + "EXTRA",        // longer than an access key id
		"/" + "AI" + "za" + fill("F", 35) + "more",   // longer than a Google key
		"da" + "pi" + hexRun(32) + "ff",              // longer than a Databricks token
		"refresh_token_" + "gh" + "p" + fill("_", 3), // nothing after the prefix
	} {
		if out, st := redact("id " + s + " end"); out != "id "+s+" end" || len(st) != 0 {
			t.Errorf("%q was redacted: %q", s, out)
		}
	}
}

// In raw JSON text, a key right after an escaped line break (\n) is still found: the "n" of the
// escape is not a letter of the key.
func TestAPIKeys_AfterJSONEscape(t *testing.T) {
	k := fakes()["openai"]
	in := `{"text":"line one\n` + k + ` and more"}`
	out, st := redact(in)
	if out != `{"text":"line one\n[REDACTED:api_key] and more"}` || st["api_key"] != 1 {
		t.Errorf("out = %q, stats %v", out, st)
	}
	// An escaped backslash before the n is a literal "\n" in the text, not an escape: the key
	// starts after a letter, and is kept.
	in2 := `{"text":"C:\\n` + k + `"}`
	if out, _ := redact(in2); out != in2 {
		t.Errorf("after an escaped backslash: %q", out)
	}
}

// Private key blocks of every kind, and a PGP key block, are replaced whole.
func TestPrivateKeys(t *testing.T) {
	for _, kind := range []string{"RSA", "EC", "OPENSSH", "ENCRYPTED", ""} {
		out, st := redact("key:\n" + pemBlock(kind) + "\nrest of the message")
		if out != "key:\n[REDACTED:private_key]\nrest of the message" || st["private_key"] != 1 {
			t.Errorf("%q: out = %q, stats %v", kind, out, st)
		}
	}
	pgp := "-----" + "BEGIN PGP " + "PRIVATE KEY BLOCK-----\nVersion: Fake 1.0\n\n" + fill("x", 30) + "\n=" + fill("y", 4) +
		"\n-----" + "END PGP " + "PRIVATE KEY BLOCK-----"
	if out, st := redact("see " + pgp + " ok"); out != "see [REDACTED:private_key] ok" || st["private_key"] != 1 {
		t.Errorf("pgp: out = %q, stats %v", out, st)
	}
}

// A PGP key block's armour headers are free text: a user id with "<email>", a URL with "?", an
// apostrophe, "_" or "#". The block is still replaced whole, as text and inside a JSON string.
func TestPrivateKeys_ArmorHeaders(t *testing.T) {
	body := mixed(64) + "\n" + mixed(64) + "\n=" + mixed(4)
	for _, header := range []string{
		"Comment: Alice Lovelace <alice@example.org>",
		"Comment: https://www.ietf.org/rfc/rfc4880.txt?x=1",
		"Comment: Bob's key",
		"Comment: user_id key #1",
		"Version: Fake 1.0\nComment: two headers <x@y.example>",
	} {
		block := "-----" + "BEGIN PGP " + "PRIVATE KEY BLOCK-----\n" + header + "\n\n" + body + "\n-----" + "END PGP " + "PRIVATE KEY BLOCK-----"
		if out, st := redact("key:\n" + block + "\nend"); out != "key:\n[REDACTED:private_key]\nend" || st["private_key"] != 1 {
			t.Errorf("%q: out = %q", header, out)
		}
		in := `{"k":"` + strings.ReplaceAll(block, "\n", `\n`) + `","n":1}`
		if out, _ := redact(in); out != `{"k":"[REDACTED:private_key]","n":1}` {
			t.Errorf("%q in JSON: out = %q", header, out)
		}
	}
	// A header holding an escaped quote in raw JSON ends the run there: the match must not
	// run out of its JSON string. What follows stays, and the JSON stays valid.
	in := `{"k":"-----` + `BEGIN PGP ` + `PRIVATE KEY BLOCK-----\nComment: say \"hi\"\n\n` + mixed(64) + `","n":1}`
	out, _ := redact(in)
	mustBeJSON(t, "the output", out)
}

// A key block inside a JSON string, with its line breaks escaped, is replaced inside the string;
// the JSON stays valid.
func TestPrivateKeys_InJSON(t *testing.T) {
	pem := strings.ReplaceAll(pemBlock("RSA"), "\n", `\n`)
	in := `{"content":"here: ` + pem + `","n":1}`
	mustBeJSON(t, "the fixture", in)
	out, st := redact(in)
	if out != `{"content":"here: [REDACTED:private_key]","n":1}` || st["private_key"] != 1 {
		t.Errorf("out = %q, stats %v", out, st)
	}
}

// A key block cut off before its END line (a truncated response) is replaced to the end of its
// body; what follows the body is kept.
func TestPrivateKeys_CutOff(t *testing.T) {
	cut := "-----" + "BEGIN RSA " + "PRIVATE KEY-----\n" + fill("M", 40) + "\n" + fill("N", 12)
	out, st := redact(`{"text":"` + strings.ReplaceAll(cut, "\n", `\n`) + `","stop":"max_tokens"}`)
	if out != `{"text":"[REDACTED:private_key]","stop":"max_tokens"}` || st["private_key"] != 1 {
		t.Errorf("out = %q, stats %v", out, st)
	}
}

// A public key or a certificate is not a private key.
func TestPrivateKeys_PublicIsKept(t *testing.T) {
	for _, k := range []string{"PUBLIC KEY", "CERTIFICATE", "RSA PUBLIC KEY"} {
		in := "-----" + "BEGIN " + k + "-----\n" + fill("M", 40) + "\n-----" + "END " + k + "-----"
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%s changed: %q", k, out)
		}
	}
}

func TestJWT(t *testing.T) {
	for _, in := range []string{"token=" + fakeJWT(), `{"id_token":"` + fakeJWT() + `"}`, "jwt " + fakeJWT() + "."} {
		out, st := redact(in)
		if !strings.Contains(out, "[REDACTED:jwt]") || strings.Contains(out, "eyJ") || st["jwt"] != 1 {
			t.Errorf("%q: out = %q, stats %v", in, out, st)
		}
	}
}

// The password in a URL is replaced; the scheme, user and host stay.
func TestURLPasswords(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"postgresql://admin:" + "s3cr3t" + "_pw@db.example.com:5432/mydb", "postgresql://admin:[REDACTED:password]@db.example.com:5432/mydb"},
		{"mongodb+srv://app:" + fill("p", 12) + "@cluster.example.net/x", "mongodb+srv://app:[REDACTED:password]@cluster.example.net/x"},
		{"see https://user:" + fill("q", 8) + "@git.example.com/repo", "see https://user:[REDACTED:password]@git.example.com/repo"},
		{"redis://:" + fill("r", 10) + "@cache:6379/0", "redis://:[REDACTED:password]@cache:6379/0"},
		{"mysql://root:" + "1234" + "56@db:3306/x", "mysql://root:[REDACTED:password]@db:3306/x"},                      // all digits, not a port
		{"postgres://bob:" + "p@ss/w0rd" + "@db.example.com/x", "postgres://bob:[REDACTED:password]@db.example.com/x"}, // both @ and /
		{"postgres://u:" + fill("a", 129) + "@db/x", "postgres://u:[REDACTED:password]@db/x"},                          // over 128 bytes

		{`{"dsn":"amqp://guest:` + fill("g", 9) + `@mq/vhost","n":1}`, `{"dsn":"amqp://guest:[REDACTED:password]@mq/vhost","n":1}`},
	} {
		out, st := redact(tc.in)
		if out != tc.want || st["password"] != 1 {
			t.Errorf("%q: out = %q, stats %v, want %q", tc.in, out, st, tc.want)
		}
	}
}

// A URL with no password, a port, or an email after it keeps everything.
func TestURLPasswords_Kept(t *testing.T) {
	for _, in := range []string{
		"postgres://localhost:5432/db",
		"https://user@host.example.com/x",
		"http://localhost:8080/api, ping ops@example.com",
		"http://h:8080?x=a@b.example.com",
		"http://host:8080 then mail a@b.example.com",
		"ssh://git@github.com/org/repo",
		"http://[::1]/contact@company.example.com", // an IPv6 host, an @ in the path
		"http://[::1]:8080/x",
		"https://not-hooks." + "slack.com/services/" + "T" + fill("S", 8) + "/B" + fill("T", 8) + "/" + fill("U", 12), // another host
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
}

// Authorization values, in a header, a JSON pair and escaped JSON, lose their token; the scheme
// stays. A bearer token anywhere is replaced, including base64's "+ / =".
func TestAuthorizationAndBearer(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("user:" + "pw"))
	b64tok := mixed(10) + "+" + mixed(10) + "/" + mixed(6) + "=="
	for _, tc := range []struct{ in, want string }{
		{"Authorization: Bearer " + fill("Z", 30), "Authorization: Bearer [REDACTED:credential]"},
		{"Authorization: Basic " + basic, "Authorization: Basic [REDACTED:credential]"},
		{`{"authorization":"Basic ` + basic + `","model":"m"}`, `{"authorization":"Basic [REDACTED:credential]","model":"m"}`},
		{`{"a":"{\"Authorization\": \"token ` + mixed(24) + `\"}"}`, `{"a":"{\"Authorization\": \"token [REDACTED:credential]\"}"}`},
		{"authorization=" + mixed(24), "authorization=[REDACTED:credential]"},
		{"use Bearer " + mixed(24) + " here", "use Bearer [REDACTED:credential] here"},
		{"Bearer " + b64tok + " end", "Bearer [REDACTED:credential] end"},
	} {
		out, st := redact(tc.in)
		if out != tc.want || st["credential"] != 1 {
			t.Errorf("%q: out = %q, stats %v, want %q", tc.in, out, st, tc.want)
		}
		if strings.HasPrefix(tc.in, "{") {
			mustBeJSON(t, "the output", out)
		}
	}
}

// A Digest Authorization value keeps its parameters and loses its response; a JWT or a quoted
// password longer than a rule's first window is still found whole.
func TestDigestAndLongValues(t *testing.T) {
	resp := hexRun(32)
	digest := `Authorization: Digest username="alice", realm="api", nonce="` + fill("n", 16) + `", uri="/x", response="` + resp + `"`
	if out, st := redact(digest); strings.Contains(out, resp) || !strings.Contains(out, `username="alice"`) || st["credential"] != 1 {
		t.Errorf("digest: out = %q, stats %v", out, st)
	}
	long := "ey" + "J" + fill("a", 16) + ".ey" + "J" + mixed(40) + fill("b", 600) + "." + fill("c", 43)
	if out, st := redact("token " + long + " end"); out != "token [REDACTED:jwt] end" || st["jwt"] != 1 {
		t.Errorf("a long JWT: out = %q", out)
	}
	pw := strings.Repeat("my long pass phrase 9, ", 14) // about 320 bytes
	if out, _ := redact(`password = "` + pw + `"`); out != `password = "[REDACTED:password]"` {
		t.Errorf("a long quoted password: out = %q", out)
	}
}

// Prose about authorization or a bearer is not a credential.
func TestAuthorizationAndBearer_Kept(t *testing.T) {
	for _, in := range []string{
		"authorization: required",
		"the bearer of bad news",
		"Bearer " + fill("x", 30), // one kind of character: not a generated token
		"Authorization header missing",
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
}
