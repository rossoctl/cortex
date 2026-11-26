package content

import (
	"encoding/json"
	"strings"
	"testing"
)

// REGRESSIONS from the third round of bot review: the query-string rescan, a JSON \uXXXX escape
// in an armour header, and a quoted mysql password.

// A rejected value with a query string is rescanned from its "?", so a password in the query is
// still found; the rescans have a total budget, so "key=?" repeated stays linear (bench_test.go).
func TestRegression_QueryStringRescan(t *testing.T) {
	pw := "dev" + "Pass42"
	in := "auth_url=https://host/" + strings.Repeat("a", 300) + "?password=" + pw
	if out, _ := redact(in); strings.Contains(out, pw) {
		t.Errorf("leaked: %q", out)
	}
	// Ordinary text with a nested redirect URL before it must not use up the budget.
	a, b := strings.Repeat("a", 300), strings.Repeat("b", 300)
	in = "auth_url=https://sso.example.io/" + a + "?next_auth=https://app.example.io/" + a + "?state=" + b + "\n" +
		" auth_url=https://host/" + a + "?password=" + pw
	if out, _ := redact(in); strings.Contains(out, pw) {
		t.Errorf("leaked after a nested URL: %q", out)
	}
	in = strings.Repeat("key=?", 100) + " auth_url=https://host/" + a + "?password=" + pw
	if out, _ := redact(in); strings.Contains(out, pw) {
		t.Errorf("leaked after query marks: %q", out)
	}
	// Redirect URLs nested 4 deep, the password in the innermost query.
	in = "auth_url=https://a.example.io/" + a + "?next_url=https://b.example.io/" + a +
		"?return_url=https://c.example.io/" + a + "?redirect_url=https://d.example.io/" + a + "?password=" + pw
	if out, _ := redact(in); strings.Contains(out, pw) {
		t.Errorf("leaked 4 deep: %q", out)
	}
	// The same with secret-named nested keys, so each level is a rescan of its own, and segments
	// long enough that the floor is small: this fails if rescanBudgetPer drops to 1 (one rescan
	// per run).
	long := strings.Repeat("a", 4000)
	in = "auth_url=https://a.example.io/" + long + "?token_url=https://b.example.io/" + long +
		"?auth_redirect=https://c.example.io/" + long + "?token_uri=https://d.example.io/" + long + "?password=" + pw
	if out, _ := redact(in); strings.Contains(out, pw) {
		t.Errorf("leaked 4 deep with secret-named keys")
	}
	// Short runs get the floor: junk glued to the URL, or many sibling redirect parameters.
	for name, in := range map[string]string{
		"glued junk":        "see " + strings.Repeat("token=?", 5) + "auth_url=https://host/" + a + "?password=" + pw,
		"sibling redirects": "auth_url=https://sso.example.io/" + strings.Repeat("?auth_cb=https://app.example.io/"+b[:40], 12) + "?password=" + pw,
	} {
		if out, _ := redact(in); strings.Contains(out, pw) {
			t.Errorf("%s: leaked", name)
		}
	}
	// Each run of rescanned text has its own budget: junk before a later URL does not use it up.
	for _, n := range []int{1000, 5000, 50000} {
		in = strings.Repeat("key=?", n) + "\n auth_url=https://host/" + a + "?password=" + pw
		if out, _ := redact(in); strings.Contains(out, pw) {
			t.Errorf("leaked after %d query marks", n)
		}
	}
	in = strings.Repeat("key=?", 200000)
	if out, _ := redact(in); out != in {
		t.Errorf("\"key=?\" repeated changed")
	}
}

// Go's JSON encoder writes "<" as a \u escape, so an armour header "Comment: Alice <alice@...>"
// holds one in raw JSON. It is part of the header: the block is replaced whole.
func TestRegression_UnicodeEscapeInArmorHeader(t *testing.T) {
	lt := string([]byte{92, 'u', '0', '0', '3', 'c'}) // the escape, built so no tool decodes it
	body := mixed(60)
	in := `{"k":"` + pgpBegin + `\nComment: Alice ` + lt + `alice@example.org>\n\n` + body + `\n` + pgpEnd + `","n":1}`
	mustBeJSON(t, "the fixture", in)
	out, _ := redact(in)
	if strings.Contains(out, body) || out != `{"k":"[REDACTED:private_key]","n":1}` {
		t.Errorf("out = %q", out)
	}
	mustBeJSON(t, "the output", out)

	// An escaped quote with no END line after it still ends the run: the string may close there.
	in = `{"k":"` + pgpBegin + `\nComment: x ` + lt + ` y\"","b":"keep me"}`
	mustBeJSON(t, "the fixture", in)
	out, _ = redact(in)
	if !strings.Contains(out, `"b":"keep me"`) {
		t.Errorf("out = %q", out)
	}
	mustBeJSON(t, "the output", out)
}

// mysql's -p takes its password attached: bare, single- or double-quoted, or double-quoted with
// escaped quotes in raw JSON. "-p" alone (mysql prompts) and quotes that are JSON's are kept.
// String on compact raw JSON with mysql's quoted password before a redirect stays valid JSON, with
// the secret gone.
func TestRegression_MysqlRedirectInCompactJSON(t *testing.T) {
	pw := hntPassword()
	for _, in := range []string{
		`[[["mysql -p"]],[["<x>"]]]`,
		`[["mysql -p\"` + pw + `\"<in.sql"],["<x>"]]`,
		`{"cmds":[["mysql -u root -p\"` + pw + `\">out.sql"]],"next":[["<y>"]]}`,
		`[["mysqldump -p'` + pw + `'>a.sql"],[">b"]]`,
		// the same gap in JSON inside a JSON string (#2125 review)
		`{"body":"[[[\"mysql -p\"]],[[\"<x>\"]]]"}`,
	} {
		out, _ := New().String(in)
		if !json.Valid([]byte(out)) || strings.Contains(out, pw) {
			t.Errorf("%s -> %s: want valid JSON without the password", in, out)
		}
	}
	// An escaped password before a redirect may start or end with what is no string gap.
	for _, p := range []string{"Secret9(" + "x", "Secret9:" + "x", ":Secret9" + "x"} {
		in := `{"cmd":"mysql -u root -p\"` + p + `\"<dump.sql"}`
		if out, _ := New().String(in); strings.Contains(out, p) || !json.Valid([]byte(out)) {
			t.Errorf("%s -> %s: want the password gone", in, out)
		}
	}
}

func TestRegression_MysqlQuotedPassword(t *testing.T) {
	pw := "Sup3r" + "S3cret!"
	for _, tc := range []struct{ in, want string }{
		{"mysql -u root -p'" + pw + "' db", "mysql -u root -p'[REDACTED:password]' db"},
		{`mysql -u root -p"` + pw + `" db`, `mysql -u root -p"[REDACTED:password]" db`},
		{`{"cmd":"mysql -u root -p\"` + pw + `\" db"}`, `{"cmd":"mysql -u root -p\"[REDACTED:password]\" db"}`},
		{"mysql -u root -p" + pw + " db", "mysql -u root -p[REDACTED:password] db"},
		{"mysql -u root -p -h db", "mysql -u root -p -h db"},
		{`{"cmd":"mysql -p","x":"y"}`, `{"cmd":"mysql -p","x":"y"}`},
		{`{"cmd": "mysql -p", "x": 1234, "y": "z"}`, `{"cmd": "mysql -p", "x": 1234, "y": "z"}`},
		{`{"cmd":"mysql -p","flag":true}`, `{"cmd":"mysql -p","flag":true}`},
		// quotes that close JSON strings, or a quoted phrase, are not a quoted password
		{`{"mysql -p":12345,"y":"z"}`, `{"mysql -p":12345,"y":"z"}`},
		{`["mysql -u root -p", 3306, "db"]`, `["mysql -u root -p", 3306, "db"]`},
		{`{"cmd":"mysql -p"  ,  "x":1}`, `{"cmd":"mysql -p"  ,  "x":1}`},
		{`["mysql -p",12345," x"]`, `["mysql -p",12345," x"]`},
		{`{"cmd":"mysql -p'","note":"don't"}`, `{"cmd":"mysql -p'","note":"don't"}`},
		{`Use "mysql -p" or "mysql --password" to log in`, `Use "mysql -p" or "mysql --password" to log in`},
		{`{"t":"Use \"mysql -p\" or \"mysql --password\""}`, `{"t":"Use \"mysql -p\" or \"mysql --password\""}`},
		{"Run 'mysql -p' and 'mysql -h' then", "Run 'mysql -p' and 'mysql -h' then"},
		// a quoted password that closes an outer quote (the usual sh -c, ssh, JSON tool call)
		{`{"cmd":"mysql -u root -p'` + pw + `'"}`, `{"cmd":"mysql -u root -p'[REDACTED:password]'"}`},
		{`{"cmd":"mysql -u root -p'` + pw + `'\nexit"}`, `{"cmd":"mysql -u root -p'[REDACTED:password]'\nexit"}`},
		{`ssh db "mysql -u root -p'` + pw + `'"`, `ssh db "mysql -u root -p'[REDACTED:password]'"`},
		{`sh -c 'mysql -u root -p"` + pw + `"'`, `sh -c 'mysql -u root -p"[REDACTED:password]"'`},
		{`{"cmd":"ssh db \"mysql -u root -p'` + pw + `'\""}`, `{"cmd":"ssh db \"mysql -u root -p'[REDACTED:password]'\""}`},
		{`{"cmd":"sh -c 'mysql -u root -p\"` + pw + `\"'"}`, `{"cmd":"sh -c 'mysql -u root -p\"[REDACTED:password]\"'"}`},
		// ... while quotes of separate strings or pieces of code are still kept
		{`{"a":"mysql -p'","b":"x'"}`, `{"a":"mysql -p'","b":"x'"}`},
		{`{"a":"mysql -p'x","b":"y'"}`, `{"a":"mysql -p'x","b":"y'"}`},
		{`["mysql -p'abcd", "efgh'"]`, `["mysql -p'abcd", "efgh'"]`},
		{`{"t":"Run \"mysql -p\" and 'x'"}`, `{"t":"Run \"mysql -p\" and 'x'"}`},
		{`cmd = 'mysql -p"' + x + '"'`, `cmd = 'mysql -p"' + x + '"'`},
		{`{"code":"cmd = 'mysql -p\"' + x + '\"'"}`, `{"code":"cmd = 'mysql -p\"' + x + '\"'"}`},
		// ... while a raw-JSON password an outer single quote closes is still found
		{`{"cmd":"sh -c 'mysql -u root -p\"` + pw + `'\" db'"}`, `{"cmd":"sh -c 'mysql -u root -p\"[REDACTED:password]\" db'"}`},
		// a redirect right after the closing quote ends the argument too
		{"mysqldump -u root -p'" + pw + "'>out.sql", "mysqldump -u root -p'[REDACTED:password]'>out.sql"},
		{`mysql -p"` + pw + `"<in.sql`, `mysql -p"[REDACTED:password]"<in.sql`},
		// ... but the gap between two strings of compact raw JSON is not a password
		{`[[["mysql -p"]],[["<x>"]]]`, `[[["mysql -p"]],[["<x>"]]]`},
		{`run("mysql -p",fn("<x"))`, `run("mysql -p",fn("<x"))`},
		{`f("mysql -p")("<x")`, `f("mysql -p")("<x")`},
		// ... while a password may start or end with a bracket
		{`mysql -p"(Secret9)"<in.sql`, `mysql -p"[REDACTED:password]"<in.sql`},
		{`mysqldump -u root -p"[abc123]">out.sql`, `mysqldump -u root -p"[REDACTED:password]">out.sql`},
		// ... but quoted code before a "<" or ">" is not a password
		{`if (s == 'mysql -p') x = '<none>'`, `if (s == 'mysql -p') x = '<none>'`},
		{`assert cmd.startswith('mysql -p') and '>' in cmd`, `assert cmd.startswith('mysql -p') and '>' in cmd`},
		{`cmd = 'mysqldump -u root -p' + pw + '>backup.sql'`, `cmd = 'mysqldump -u root -p' + pw + '>backup.sql'`},
		// a double-quoted password may still start or end with a single quote
		{`mysql -p"'` + pw + `"`, `mysql -p"[REDACTED:password]"`},
		{`mysql -p"` + pw + `'" db`, `mysql -p"[REDACTED:password]" db`},
		// the first -p is the one taken
		{"mysql -p" + pw + " -h db -p'x'", "mysql -p[REDACTED:password] -h db -p'x'"},
		{"mysql -u root -p -h db -p" + pw, "mysql -u root -p -h db -p[REDACTED:password]"},
	} {
		out, _ := redact(tc.in)
		if out != tc.want {
			t.Errorf("%q: out = %q, want %q", tc.in, out, tc.want)
		}
		if strings.HasPrefix(tc.in, "{") {
			mustBeJSON(t, "the output", out)
		}
	}
}
