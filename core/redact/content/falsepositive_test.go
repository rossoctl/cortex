package content

import (
	"encoding/base64"
	"math/rand/v2"
	"strings"
	"testing"
)

// THE FALSE-POSITIVE CORPUS: content shaped like what a coding agent's traffic carries, holding
// no secret. Every rule must leave all of it alone: a false positive corrupts a tool's input or
// a diff in the export.
func falsePositiveCorpus() map[string]string {
	sha := hexRun(40)
	return map[string]string{
		"task ids":        `{"task_id":"task-` + "a1b2c3d4e5f6a7b8c9d0" + `","subtask":"task-` + mixed(24) + `"}`,
		"git":             `{"commit":"` + sha + `","parent":"` + sha[:7] + `","message":"fix: handle the ask-for-review flow"}`,
		"trace ids":       `{"trace_id":"` + hexRun(32) + `","span_id":"` + hexRun(16) + `","request_id":"req_` + mixed(24) + `"}`,
		"tool ids":        `{"type":"tool_use","id":"toolu_` + mixed(24) + `","name":"Bash","input":{"command":"ls -la"}}`,
		"computer use":    `{"action":"key","text":"Return"} {"action":"key","text":"ctrl+shift+t"}`,
		"cache keys":      `{"cache_key":"user:1234:profile","sort_key":"2026-11-22T10:00:00Z","primary_key":"id"}`,
		"lockfile":        `"integrity": "sha512-` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x00\x01\x02\xfe\xff", 13))) + `"`,
		"usage":           `{"usage":{"input_tokens":12345,"output_tokens":678,"cache_read_input_tokens":90123,"max_tokens":4096}}`,
		"thinking":        `{"type":"thinking","thinking":"Let me check the auth flow first.","signature":"` + mixed(60) + `"}`,
		"prose":           "The password reset flow sends a token by email; the user's secret question is asked after that. Bearer tokens expire.",
		"code":            "func login(password string) error {\n\tif password == \"\" {\n\t\treturn errEmpty\n\t}\n\ttoken := newToken()\n\tkey := cfg.Key\n}",
		"typescript":      "interface Login { password: string; apiKey?: string; token: Token }\nconst secret = process.env.SECRET;",
		"env template":    "DB_PASSWORD=${DB_PASSWORD}\nAPI_KEY=<your-api-key>\nSECRET_KEY=changeme\nPWD=/home/dev/project",
		"urls":            "see https://docs.example.com:8443/auth/token?client=web#keys and http://localhost:3000/api",
		"emails":          "mail ops@example.com or git@github.com:org/repo.git",
		"paths":           `C:\Users\dev\src\project\secrets\README.md and /etc/ssl/private/README`,
		"uuids":           `{"session":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0","request_id":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0","idempotency_key":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}`,
		"hashes":          `{"cache_key":"` + hexRun(64) + `","Idempotency-Key":"` + hexRun(32) + `","etag":"` + hexRun(40) + `"}`,
		"typescript decl": "passwordInput: HTMLInputElement;\nconst passwordField: PasswordField = form.field;",
		"error messages":  "Error: password: authentication failed for user\ntoken: expired at noon",
		"pass words":      "passport=AB1234567X compass_heading=north bypass_cache=enabled",
		"authorization":   "Authorization header is required; authorization: pending",
		"words with keys": "monkey keyboard turkey passed bypass author authority tokens secrets",
		"css and html":    `<input type="password" name="password" autocomplete="current-password">`,
		"line ends":       "Changed the password:\nreset_password_for_user_account()\nand the token =\n" + mixed(24) + "\n",
	}
}

func TestFalsePositives(t *testing.T) {
	for name, in := range falsePositiveCorpus() {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%s: changed (stats %v):\n in: %q\nout: %q", name, st, in, out)
		}
	}
}

// Random base64 (an image's data, as raw JSON) has no secret in it; none may be found, however
// many prefixes appear in it by chance.
func TestFalsePositives_RandomBase64(t *testing.T) {
	for _, in := range []string{randomImageJSON(4 << 20), randomImageJSON(4<<20, 76)} {
		if out, st := redact(in); len(st) != 0 {
			t.Errorf("found %v in random base64 (%d bytes)", st, len(out))
		}
	}
}

// randomImageJSON is an Anthropic request with an image block of about n bytes of base64, from a
// fixed seed. With wrap > 0 the data is cut into lines of wrap characters, as MIME base64 is:
// each line is then a run the encoded rule decodes, the worst case for that rule.
func randomImageJSON(n int, wrap ...int) string {
	rng := rand.New(rand.NewPCG(1, 2))
	raw := make([]byte, n*3/4)
	for i := range raw {
		raw[i] = byte(rng.Uint32())
	}
	data := base64.StdEncoding.EncodeToString(raw)
	if len(wrap) > 0 {
		var b strings.Builder
		for i := 0; i < len(data); i += wrap[0] {
			b.WriteString(data[i:min(len(data), i+wrap[0])])
			b.WriteString(`\n`)
		}
		data = b.String()
	}
	return `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		data + `"}},{"type":"text","text":"what is in this image?"}]}]}`
}
