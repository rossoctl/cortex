package content

import (
	"strings"
	"testing"
)

// REGRESSIONS for identifiers, URL lists, tool names and code that must be kept, and for the
// command-line forms (curl, docker, mysql, --password) that must be found.

func TestRegression_IdentifiersAndCommandsKept(t *testing.T) {
	for _, in := range []string{
		// identifiers with a one-letter chunk
		"token_provider = GoogleOAuth2AuthenticationProvider",
		"auth_fn: getXCoordinateFromToken",
		"token_kind=iOSPushNotificationToken",
		"auth_flow: OAuth2ClientCredentials",
		// a list of URLs: a port, then the next URL
		"ELASTICSEARCH_HOSTS=http://es1:9200,http://elastic@es2:9200",
		// another tool whose name starts with mysql
		"mysql-workbench -pAbc12345",
		// calls, generics, API docs, an environment variable's name
		"password = hash_password(raw)",
		"const passwordRef = useRef<HTMLInputElement>(null)",
		"password: Required. The user's password.",
		"token: Deprecated, use api_key instead",
		"password_env: DB_PASSWORD",
		// a documented limit: one capitalized word is read as a type name
		"password=TrustNoOneEver",
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q, stats %v", in, out, st)
		}
	}
}

func TestRegression_CommandsFound(t *testing.T) {
	pw := fakePassword()
	for name, tc := range map[string]struct{ in, secret string }{
		"curl combined flags":      {"curl -sSu admin:" + pw + " https://api.example.com/x", pw},
		"curl --user=":             {"curl --user=admin:" + pw + " https://api.example.com/x", pw},
		"--password quoted":        {`tool --password "my pass 123" --verbose`, "my pass 123"},
		"--password=":              {"tool --password=" + pw, pw},
		"docker login --password=": {"docker login --password=" + pw + " registry.example.com", pw},
		"url userinfo, UTF-8":      {"postgres://bob:p" + "ässwörd1@db.example.com/x", "ässwörd1"},
		"curl quoted user":         {"curl -u 'alice:one " + "two3' https://api.example.com/x", "two3"},
	} {
		out, st := redact(tc.in)
		if strings.Contains(out, tc.secret) || len(st) == 0 {
			t.Errorf("%s: leaked: %q (stats %v)", name, out, st)
		}
	}
}

// In raw JSON text a command-line value stops before a JSON escape: the escape stays, and the
// JSON stays valid.
func TestRegression_CommandsInJSONKeepEscapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"cmd":"mysql -p` + "abc" + `123\" tail"}`, `{"cmd":"mysql -p[REDACTED:password]\" tail"}`},
		{`{"cmd":"tool --password ` + fakePassword() + `\"x"}`, `{"cmd":"tool --password [REDACTED:password]\"x"}`},
		{`{"cmd":"curl -u admin:` + fakePassword() + `\" https://x"}`, `{"cmd":"curl -u admin:[REDACTED:password]\" https://x"}`},
	} {
		mustBeJSON(t, "the fixture", tc.in)
		out, _ := redact(tc.in)
		if out != tc.want {
			t.Errorf("%q: out = %q, want %q", tc.in, out, tc.want)
		}
		mustBeJSON(t, "the output", out)
	}
}
