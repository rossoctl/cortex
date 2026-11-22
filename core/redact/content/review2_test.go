package content

import (
	"encoding/base64"
	"strings"
	"testing"
)

// REGRESSIONS from the second review: rule gaps found by probes against the text-value rules.
// Each case names the secret that must be gone; the output must also be valid JSON when the
// input is.

// fakeDjangoKey is 50 characters from Django's generated alphabet, punctuation included.
func fakeDjangoKey() string {
	const a = "abcdefghij" + "0123456789" + "!@#$%^&*(-_=+)"
	b := make([]byte, 50)
	for i := range b {
		b[i] = a[(i*7)%len(a)]
	}
	return string(b)
}

func TestReview2_Found(t *testing.T) {
	pw, v, uuid := fakePassword(), mixed(24), "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	envB64 := base64.StdEncoding.EncodeToString([]byte("DB_HOST=db.example.com\nDB_PASSWORD=" + pw + "\nDEBUG=false\n"))
	dockerCfg := base64.StdEncoding.EncodeToString([]byte(`{"auths":{"registry.example.com":{"username":"me","password":"` + pw + `"}}}`))
	dj := fakeDjangoKey()
	sig := hexRun(64)
	for name, tc := range map[string]struct{ in, secret string }{
		// 1. "pass" is a password word
		"DB_PASS":   {"DB_PASS=" + pw, pw},
		"SMTP_PASS": {"SMTP_PASS: '" + pw + "'", pw},
		"yaml pass": {"user: me\npass: " + pw + "\n", pw},
		// 3. a rejected value does not hide the assignment inside it
		"password in a URL query":      {"auth_url=https://x.example.io/login?password=" + pw, pw},
		"client_secret in a URL query": {"token_url=https://auth.example.com/token?client_secret=" + v, v},
		// 4. the keyword rule runs on decoded base64
		"base64 env file":      {"data: " + envB64, envB64},
		"base64 docker config": {`{".dockerconfigjson":"` + dockerCfg + `"}`, dockerCfg},
		// 5. a URL password with "@ / #" in it
		"url password with @":       {"postgres://bob:p@ssw0rd@db.example.com/x", "p@ssw0rd"},
		"url password with / and #": {"mysql://u:pa/ss#1@db:3306/x", "pa/ss#1"},
		// 6. command-line forms
		"--password":   {"tool --password " + pw + " --verbose", pw},
		"--token":      {"gh auth login --token " + v, v},
		"docker login": {"docker login -u me -p " + pw + " registry.example.com", pw},
		"mysql -p":     {"mysql -u root -p" + pw + " shop", pw},
		"curl -u":      {"curl -u admin:" + pw + " https://api.example.com/x", pw},
		// 7. a UUID under a secret name is a secret
		"uuid api key":  {"HEROKU_API_KEY=" + uuid, uuid},
		"uuid json key": {`{"api_key":"` + uuid + `"}`, uuid},
		// 8. Django's SECRET_KEY, punctuation and prefix included
		"django insecure": {"SECRET_KEY = 'django-insecure-" + dj + "'", dj},
		"django":          {`SECRET_KEY = "` + dj + `"`, dj},
		// low: other schemes, SigV4, quoted passwords with spaces
		"negotiate":                   {"Authorization: Negotiate " + mixed(40), mixed(40)},
		"ntlm":                        {"Authorization: NTLM " + mixed(40), mixed(40)},
		"sigv4 header":                {"Authorization: AWS4-HMAC-SHA256 Credential=" + "AK" + "IA" + fill("E", 16) + "/20261122/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + sig, sig},
		"sigv4 url":                   {"https://bucket.s3.amazonaws.com/x?X-Amz-Credential=a&X-Amz-Signature=" + sig + "&x=1", sig},
		"quoted password with spaces": {`password = "my secret 123"`, "my secret 123"},
	} {
		out, st := redact(tc.in)
		if strings.Contains(out, tc.secret) || len(st) == 0 {
			t.Errorf("%s: leaked: %q (stats %v)", name, out, st)
		}
		if strings.HasPrefix(tc.in, "{") {
			mustBeJSON(t, name, out)
		}
	}
}

// What the new forms must keep: words around "pass", help text, prompts, a port before an email,
// ids under a bare key name.
func TestReview2_Kept(t *testing.T) {
	for _, in := range []string{
		"passport=AB1234567X", "compass_heading=north-east-2", "bypass_cache=enabled",
		"use the --password option to set it",
		"--password string   the password to use",
		"--token-file /run/secrets/token",
		"mysql -u root -p shop",                 // with a space, mysql prompts
		"curl -u admin https://api.example.com", // no password: curl prompts
		"docker login -p",
		"http://host:8080/contact#ops@example.com",
		"Idempotency-Key: 0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		`{"cache_key":"` + hexRun(64) + `"}`,
		"passwordInput: HTMLInputElement;",
		"Error: password: authentication failed",
		`password = "correct horse battery"`, // words only: a known limit, kept as prose
		"password=hu***22",                   // a masked value
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
}
