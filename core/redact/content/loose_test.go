package content

import (
	"strings"
	"testing"
)

// A key with an ANSI escape, a format character or a combining mark inside it is found, and
// replaced whole, the runes inside included; text without them is as String has it.
func TestStringSkipping(t *testing.T) {
	key := fakes()["github"]
	for _, in := range []string{
		"token \x1b[1m" + key + "\x1b[0m please",
		"use " + key[:10] + "​" + key[10:],
		"use " + key[:10] + "­" + key[10:],
		"use " + key[:10] + "́" + key[10:],
		"use " + key[:4] + "\x1b[31m" + key[4:],
	} {
		out, st := New().StringSkipping(in)
		if strings.Contains(out, "GGGG") || st[catAPIKey] != 1 {
			t.Errorf("%q -> %q, %v", in, out, st)
		}
	}
	if out, _ := New().StringSkipping("use " + key + " now"); out != "use [REDACTED:api_key] now" {
		t.Errorf("plain: %q", out)
	}
	for _, in := range []string{"café crème", "plain \x1b[1mbold\x1b[0m text", "a​b"} {
		if out, st := New().StringSkipping(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q", in, out)
		}
	}
}

// A key prefix at the end of text cut there is replaced; elsewhere, or with no known prefix, kept.
func TestRedactCutKey(t *testing.T) {
	for in, want := range map[string]string{
		"push with gh" + "p_GxGx7":    "push with [REDACTED:api_key]",
		"use sk-" + "ant-api03-Ab":    "use [REDACTED:api_key]",
		"(AK" + "IAEXAMPL":            "([REDACTED:api_key]",
		"gh" + "p_":                   "[REDACTED:api_key]",
		"push the branch":             "push the branch",
		"see gh" + "p_GxGx7 and more": "see gh" + "p_GxGx7 and more",
		"install mask-" + "rcnn":      "install mask-" + "rcnn",
		"set DB_PASSWORD=Sun":         "set DB_PASSWORD=[REDACTED:password]",
		"api_key: \"ab":               "api_key: \"[REDACTED:secret_value]",
		"set retries=12":              "set retries=12",
		"key sk" + "_live_ab":         "key [REDACTED:api_key]",
		"key rk" + "_test_ab":         "key [REDACTED:api_key]",
		"key dapi1f":                  "key [REDACTED:api_key]",
	} {
		if got, _ := RedactCutKey(in); got != want {
			t.Errorf("RedactCutKey(%q) = %q, want %q", in, got, want)
		}
	}
}
