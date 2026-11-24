package content

import (
	"encoding/base64"
	"strings"
	"testing"
)

// REGRESSIONS for raw JSON: inputs that corrupted JSON or leaked in a first version of these
// rules. Each is raw JSON or text, redacted whole; JSON must stay valid JSON, and only the
// secret may change.

// A rule's match must not run from one JSON value into the next.
func TestRegression_MatchesStayInsideAValue(t *testing.T) {
	for _, in := range []string{
		`{"dsn":"postgres://localhost/db","port":5432,"email":"x@y.io"}`,                                    // a URL with no credentials, then an email
		`{"url":"http://localhost:8080","contact":"ops@example.com"}`,                                       // a port, then an email
		`{"a":"http://h:8080","b":"c@d.example.com"}`,                                                       // the same, compact
		`{"a":"-----` + `BEGIN RSA ` + `PRIVATE KEY-----","b":"hello","c":"-----END RSA PRIVATE KEY-----"}`, // a header and a footer in different strings
		`{"note":"password:","value":"` + fakePassword() + `"}`,                                             // a keyword ending one string
		`{"k":"token=","v":"` + mixed(24) + `"}`,                                                            // the same, with "="
	} {
		mustBeJSON(t, "the fixture", in)
		out, _ := redact(in)
		mustBeJSON(t, "the output of "+in, out)
		for _, keep := range []string{`"port":5432`, `"email":"x@y.io"`, `"contact":"ops@example.com"`, `"b":"c@d.example.com"`, `"b":"hello"`, `"value":"` + fakePassword() + `"`, `"v":"` + mixed(24) + `"`} {
			if strings.Contains(in, keep) && !strings.Contains(out, keep) {
				t.Errorf("%q lost %s: %q", in, keep, out)
			}
		}
	}
}

// An Authorization value in compact JSON is replaced alone: the rest of the document stays.
func TestRegression_AuthorizationInCompactJSON(t *testing.T) {
	tok := mixed(30)
	in := `{"headers":{"Authorization":"Bearer ` + tok + `","X-Trace":"abc"},"model":"m","stream":true}`
	want := `{"headers":{"Authorization":"Bearer [REDACTED:credential]","X-Trace":"abc"},"model":"m","stream":true}`
	if out, _ := redact(in); out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

// Secrets the first version let through.
func TestRegression_Leaks(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("admin:" + fakePassword()))
	for name, tc := range map[string]struct{ in, secret string }{
		"aws secret env":      {"AWS_SECRET_ACCESS_KEY=" + mixed(40), mixed(40)},
		"secret_key quoted":   {"SECRET_KEY='" + mixed(32) + "'", mixed(32)},
		"short password":      {`{"password":"` + fakePassword() + `"}`, fakePassword()},
		"hex token":           {"api_token=" + hexRun(32), hexRun(32)},
		"json authorization":  {`{"authorization":"Basic ` + basic + `"}`, basic},
		"openai svcacct":      {"k " + fakes()["openai-svcacct"], fakes()["openai-svcacct"]},
		"openai admin":        {"k " + fakes()["openai-admin"], fakes()["openai-admin"]},
		"slack refresh":       {"k " + fakes()["slack-refresh"], fakes()["slack-refresh"]},
		"slack app":           {"k " + fakes()["slack-app"], fakes()["slack-app"]},
		"stripe test":         {"k " + fakes()["stripe-test"], fakes()["stripe-test"]},
		"bearer with + / =":   {"Bearer " + mixed(12) + "+" + mixed(12) + "/" + mixed(8) + "==", mixed(8) + "=="},
		"after a json escape": {`{"t":"a\n` + fakes()["github"] + `"}`, fakes()["github"]},
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
