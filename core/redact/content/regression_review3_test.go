package content

import (
	"strings"
	"testing"
)

// From the hostile review of #2125 at d6521b83: leaks through JSON() that Bytes() caught, and one
// both missed.

// A key name with a space is a name: "API Key", "Root Pass", as a key and in tool arguments.
func TestReview3_KeyNamesWithSpaces(t *testing.T) {
	tok, pw := mixed(32), hntPassword()
	for _, tc := range []struct{ in, secret string }{
		{`{"API Key": "` + tok + `"}`, tok},
		{`{"Secret Key": "` + tok + `"}`, tok},
		{`{"Access Key": "` + tok + `"}`, tok},
		{`{"api key": "` + tok + `"}`, tok},
		{`{"Root Pass": "` + pw + `"}`, pw},
		{`{"Admin PWD": "` + pw + `"}`, pw},
		{`{"Wallet Passphrase": "` + pw + `"}`, pw},
		// Inside a tool call's arguments, a JSON document in a string.
		{`{"input": "{\"API Key\": \"` + tok + `\"}"}`, tok},
	} {
		out, _ := redactJSON(tc.in)
		if strings.Contains(out, tc.secret) {
			t.Errorf("JSON %s: leaked: %s", tc.in, out)
		}
		mustBeJSON(t, tc.in, out)
		if b, _ := New().Bytes([]byte(tc.in)); strings.Contains(string(b), tc.secret) {
			t.Errorf("Bytes %s: leaked: %s", tc.in, b)
		}
	}
	// Names that hold no secret word stay names: kept.
	for _, in := range []string{`{"Display Name": "Hnt7rQx2Lp"}`, `{"Key Board": "Hnt7rQx2Lp"}`} {
		if out, _ := redactJSON(in); out != in {
			t.Errorf("%s changed: %s", in, out)
		}
	}
}

// A keyed value with a line break before or after it is the same secret; the break is kept.
func TestReview3_KeyedValueWithALineBreak(t *testing.T) {
	tok, pw := mixed(32), hntPassword()
	for _, tc := range []struct{ in, want string }{
		{`{"password": "` + pw + `\n"}`, `{"password": "[REDACTED:password]\n"}`},
		{`{"password": "\n` + pw + `"}`, `{"password": "\n[REDACTED:password]"}`},
		{`{"token": "` + tok + `\n"}`, `{"token": "[REDACTED:secret_value]\n"}`},
		{`{"client_secret": "` + tok + `\r\n"}`, `{"client_secret": "[REDACTED:secret_value]\r\n"}`},
	} {
		out, _ := redactJSON(tc.in)
		if out != tc.want {
			t.Errorf("JSON %s\n got %s\nwant %s", tc.in, out, tc.want)
		}
		secret := pw
		if strings.Contains(tc.in, tok) {
			secret = tok
		}
		if b, _ := New().Bytes([]byte(tc.in)); strings.Contains(string(b), secret) {
			t.Errorf("Bytes %s: leaked: %s", tc.in, b)
		}
	}
}
