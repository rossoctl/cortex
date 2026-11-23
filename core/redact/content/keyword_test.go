package content

import (
	"strings"
	"testing"
)

// A secret-named keyword with a secret-shaped value, in each syntax: only the value is replaced,
// and JSON stays valid.
func TestKeywordValue(t *testing.T) {
	pw, v, h := fakePassword(), mixed(24), hexRun(40)
	for _, tc := range []struct{ in, want string }{
		// password names: a short, low-entropy value is enough
		{"password=" + pw, "password=[REDACTED:password]"},
		{"DB_PASSWORD: '" + pw + "'", "DB_PASSWORD: '[REDACTED:password]'"},
		{`{"password": "` + pw + `"}`, `{"password": "[REDACTED:password]"}`},
		{`{"a":"{\"pwd\":\"` + pw + `\"}"}`, `{"a":"{\"pwd\":\"[REDACTED:password]\"}"}`},
		{`password := "` + pw + `"`, `password := "[REDACTED:password]"`},
		{`'passphrase' => '` + pw + `'`, `'passphrase' => '[REDACTED:password]'`},
		{"--db-password=" + pw + " --verbose", "--db-password=[REDACTED:password] --verbose"},
		{"dbpassword=" + pw, "dbpassword=[REDACTED:password]"},
		// secret names: a generated-looking value
		{"export AWS_SECRET_ACCESS_KEY=" + mixed(40), "export AWS_SECRET_ACCESS_KEY=[REDACTED:secret_value]"},
		{"SECRET_KEY='" + v + "'", "SECRET_KEY='[REDACTED:secret_value]'"},
		{`{"client_secret": "` + v + `"}`, `{"client_secret": "[REDACTED:secret_value]"}`},
		{`{"apiKey":"` + v + `"}`, `{"apiKey":"[REDACTED:secret_value]"}`},
		{`{"x-api-key":"` + v + `"}`, `{"x-api-key":"[REDACTED:secret_value]"}`},
		{"api_token=" + h, "api_token=[REDACTED:secret_value]"},
		{`{"private_key_id": "` + h + `"}`, `{"private_key_id": "[REDACTED:secret_value]"}`},
		{"auth_token = " + v + ", next", "auth_token = [REDACTED:secret_value], next"},
		{"authtoken: " + v, "authtoken: [REDACTED:secret_value]"},
		{`{"a":"{\"refresh_token\":\"` + v + `\"}"}`, `{"a":"{\"refresh_token\":\"[REDACTED:secret_value]\"}"}`},
		// a bare "key" name: a longer generated value
		{`{"cache_key":"` + mixed(28) + `"}`, `{"cache_key":"[REDACTED:secret_value]"}`},
	} {
		out, st := redact(tc.in)
		if out != tc.want || len(st) != 1 {
			t.Errorf("%q: out = %q, stats %v, want %q", tc.in, out, st, tc.want)
		}
		if strings.HasPrefix(tc.in, "{") {
			mustBeJSON(t, "the fixture", tc.in)
			mustBeJSON(t, "the output", out)
		}
	}
}

// A keyword's value that is a placeholder, a reference, a path, a known id, too short or too
// plain is kept.
func TestKeywordValue_Kept(t *testing.T) {
	for _, in := range []string{
		"password=short",            // under 6
		"password=" + fill("a", 30), // one repeated character
		"password: string",          // a type
		"password=self.password",    // a reference
		"password: ${DB_PASSWORD}",  // a variable
		"PASSWORD=$PASSWORD",        // a variable
		"password=%DB_PASS%",        // a variable
		"password=changeme",         // a placeholder
		`password: "********"`,      // a mask
		"password: <your password>", // a placeholder
		"PWD=/home/user/project",    // the working directory
		"OLDPWD=~/src",              // the previous one
		"token=" + mixed(12),        // too short
		"Idempotency-Key: 0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",        // a UUID under a bare key name: an id
		"token=" + "toolu_" + "bdrk_" + mixed(24),                      // a tool-use id
		`{"token_type": "Bearer"}`,                                     // a scheme name
		`{"api_key": "your-api-key-here"}`,                             // a placeholder
		`{"auth_url": "https://login.example.com/oauth2/v2.0/token"}`,  // a URL
		`{"secretName": "my-k8s-secret-name"}`,                         // words
		"const apiKey = process.env.API_KEY;",                          // a reference
		"secret = get_secret_value_from_vault",                         // one kind of character
		`{"cache_key":"user:` + "1234:profile:settings:v2" + `"}`,      // a cache key's separators
		`{"primary_key": "user_profile_settings_id"}`,                  // words
		"the password is shown on screen and nobody types it in again", // prose
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
}

// Names that hold a secret word as part of another word are not secret names.
func TestKeywordValue_NotSecretNames(t *testing.T) {
	v := mixed(30)
	for _, name := range []string{"author", "passed", "bypass_flag", "signature", "max_tokens", "sessionId", "monkey_id", "keyboard"} {
		in := `{"` + name + `":"` + v + `"}`
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%s: %q, stats %v", name, out, st)
		}
	}
}

func TestNameTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		want tier
	}{
		{"password", tierPassword}, {"DB_PASSWORD", tierPassword}, {"userPassword", tierPassword}, {"pwd", tierPassword},
		{"passphrase", tierPassword}, {"AWS_SECRET_ACCESS_KEY", tierSecret}, {"apiKey", tierSecret}, {"APIKey", tierSecret},
		{"x-api-key", tierSecret}, {"client_secret", tierSecret}, {"accessToken", tierSecret}, {"authtoken", tierSecret},
		{"Authorization", tierSecret}, {"private_key", tierSecret}, {"cookie", tierSecret}, {"cache_key", tierKey},
		{"key", tierKey}, {"author", tierNone}, {"signature", tierNone}, {"max_tokens", tierNone}, {"keyboard", tierNone},
		{"passed", tierNone}, {"session_id", tierNone},
		// a trailing digit is not part of the word (Django's password1 and password2, TOKEN2)
		{"password1", tierPassword}, {"password2", tierPassword}, {"newPassword2", tierPassword}, {"pwd1", tierPassword},
		{"token2", tierSecret}, {"api_key2", tierSecret}, {"API_KEY_2", tierSecret}, {"key2", tierKey},
		{"sha256", tierNone}, {"utf8", tierNone}, {"v2", tierNone},
		// ... but not for "pass" and "auth": compiler passes, two-pass encodes, Auth0
		{"pass1", tierNone}, {"test_pass2", tierNone}, {"auth0", tierNone}, {"AUTH0_CLIENT_ID", tierNone},
	} {
		if got := nameTier(tc.name); got != tc.want {
			t.Errorf("nameTier(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A name whose last word says what the value is about (password_policy, token_type) keeps a
// setting: it loses the password tier's no-entropy rule. A generated token under such a name is
// still a secret.
func TestKeywordValue_ModifierNames(t *testing.T) {
	for _, in := range []string{
		"password_policy: strong",
		`{"passwordPolicy":"strong"}`,
		"password_min_length: 12345678",
		`{"password_reset_enabled": "enabled"}`,
		`{"token_type": "refresh_token"}`,
		"token_endpoint: https://auth.example.com/oauth/token",
		`{"secret_name": "github-actions-deploy"}`,
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
	for _, in := range []string{
		"api_token_name: " + mixed(40),
		"GITHUB_TOKEN_VAR=" + mixed(40),
		"x-auth-token-version: " + mixed(40),
		"db_password_env: " + mixed(16),
		"password_hint: " + mixed(16),
	} {
		if out, st := redact(in); strings.Contains(out, mixed(16)) || st["secret_value"] != 1 {
			t.Errorf("%q: out = %q, stats %v, want the token redacted", in, out, st)
		}
	}
}

// Every cookie of a Cookie header is checked, in text and in JSON; a password that only starts
// with "your" is a password.
func TestKeywordValue_CookiesAndYourPrefix(t *testing.T) {
	sid, sess := mixed(32), hexRun(32)
	for _, tc := range []struct{ in, want string }{
		{"Cookie: sid=" + sid + "; session=" + sess, "Cookie: sid=[REDACTED:secret_value]; session=[REDACTED:secret_value]"},
		{`{"cookie":"sid=` + sid + `; theme=dark"}`, `{"cookie":"sid=[REDACTED:secret_value]; theme=dark"}`},
		{"Set-Cookie: sid=" + sid + "; Path=/; HttpOnly", "Set-Cookie: sid=[REDACTED:secret_value]; Path=/; HttpOnly"},
		{"password=your" + "H0rse!2026", "password=[REDACTED:password]"},
	} {
		if out, _ := redact(tc.in); out != tc.want {
			t.Errorf("%q: out = %q, want %q", tc.in, out, tc.want)
		}
	}
}

// A value under a JSON key the caller names is checked whole: a password may then hold spaces,
// and an Authorization value keeps its scheme.
func TestKeyedValue(t *testing.T) {
	r := New()
	basic := "dXNl" + "cjpwdw=="
	for _, tc := range []struct{ key, text, want string }{
		{"password", fakePassword(), "[REDACTED:password]"},
		{"passphrase", "correct horse battery staple", "[REDACTED:password]"},
		{"client_secret", mixed(24), "[REDACTED:secret_value]"},
		{"x-api-key", " " + mixed(24) + " ", " [REDACTED:secret_value] "},
		{"Authorization", "Bearer " + mixed(24), "Bearer [REDACTED:credential]"},
		{"authorization", "Basic " + basic, "Basic [REDACTED:credential]"},
		// a password that starts like a scheme is still a password, checked whole
		{"password", "token My passphrase!", "[REDACTED:password]"},
		// every cookie of a list, not only the first
		{"cookie", "sid=" + mixed(32) + "; csrftoken=" + hexRun(32) + "; theme=dark", "sid=[REDACTED:secret_value]; csrftoken=[REDACTED:secret_value]; theme=dark"},
	} {
		e := r.Find(tc.text, Context{Key: tc.key})
		if out := Apply(tc.text, e); out != tc.want {
			t.Errorf("%s=%q: out = %q, want %q", tc.key, tc.text, out, tc.want)
		}
	}
	for _, tc := range []struct{ key, text string }{
		{"key", "Return"},
		{"Idempotency-Key", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"},
		{"cache_key", hexRun(64)},
		{"secret_note", "remember to rotate the keys"},
		{"password", "short"},
		{"signature", mixed(40)},
		{"name", mixed(40)},
	} {
		if e := r.Find(tc.text, Context{Key: tc.key}); e != nil {
			t.Errorf("%s=%q was redacted: %v", tc.key, tc.text, e)
		}
	}
}
