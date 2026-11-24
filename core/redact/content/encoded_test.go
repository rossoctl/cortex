package content

import (
	"encoding/base64"
	"testing"
)

// A secret inside base64 is found by decoding, and the whole run is replaced.
func TestEncodedSecrets(t *testing.T) {
	for name, tc := range map[string]struct {
		secret string
		enc    *base64.Encoding
	}{
		"api key":           {fakes()["anthropic"], base64.StdEncoding},
		"api key, url-safe": {fakes()["anthropic"], base64.RawURLEncoding},
		"private key":       {pemBlock("RSA"), base64.StdEncoding},
		"jwt":               {fakeJWT() + " and some text", base64.StdEncoding},
		"url password":      {"postgresql://admin:" + fill("w", 16) + "@db.example.com:5432/mydb", base64.StdEncoding},
	} {
		e := tc.enc.EncodeToString([]byte(tc.secret))
		if len(e) < 64 {
			t.Fatalf("%s: the fixture encodes to %d characters, under the rule's 64", name, len(e))
		}
		out, st := redact("value=" + e + " end")
		if out != "value=[REDACTED:encoded_secret] end" || st["encoded_secret"] != 1 {
			t.Errorf("%s: out = %q, stats %v", name, out, st)
		}
	}
}

// Base64 that decodes to no secret or to binary, base64 under 64 characters, and a long run that
// is not valid base64 are all kept. So is a run longer than the rule decodes.
func TestEncoded_Kept(t *testing.T) {
	bin := make([]byte, 96)
	for i := range bin {
		bin[i] = byte(i * 37)
	}
	long := base64.StdEncoding.EncodeToString(append([]byte(fakes()["github"]+" "), make([]byte, maxEncodedRun)...))
	for name, in := range map[string]string{
		"prose":     base64.StdEncoding.EncodeToString([]byte("This is just a regular paragraph of text that is long enough for the rule.")),
		"short":     base64.StdEncoding.EncodeToString([]byte("short")),
		"not b64":   fill("A", 65), // 65 characters: one more than a whole number of groups, so it does not decode
		"binary":    base64.StdEncoding.EncodeToString(bin),
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(bin[:64]),
		"too long":  long,
	} {
		if out, st := redact(`{"x":"` + in + `"}`); out != `{"x":"`+in+`"}` || len(st) != 0 {
			t.Errorf("%s changed (stats %v)", name, st)
		}
	}
}
