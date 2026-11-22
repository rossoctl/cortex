package content

import (
	"strings"
	"testing"
)

// REGRESSIONS from the third review: false positives in identifiers, URL lists, tool names and
// code, and the last command-line leaks.

func TestReview3_Kept(t *testing.T) {
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

func TestReview3_Found(t *testing.T) {
	pw := fakePassword()
	for name, tc := range map[string]struct{ in, secret string }{
		"curl combined flags":      {"curl -sSu admin:" + pw + " https://api.example.com/x", pw},
		"curl --user=":             {"curl --user=admin:" + pw + " https://api.example.com/x", pw},
		"--password quoted":        {`tool --password "my pass 123" --verbose`, "my pass 123"},
		"--password=":              {"tool --password=" + pw, pw},
		"docker login --password=": {"docker login --password=" + pw + " registry.example.com", pw},
		"url userinfo, UTF-8":      {"postgres://bob:p" + "ässwörd1@db.example.com/x", "ässwörd1"},
	} {
		out, st := redact(tc.in)
		if strings.Contains(out, tc.secret) || len(st) == 0 {
			t.Errorf("%s: leaked: %q (stats %v)", name, out, st)
		}
	}
}
