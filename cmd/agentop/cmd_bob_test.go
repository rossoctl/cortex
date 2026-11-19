package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// bobSettings is a plausible IBM Bob settings.json: flat dotted keys, which is the
// whole shape difference from Claude Code's nested "env" block. Deliberately long
// because TestBobEnable_PreservesEverythingElse is the test that matters most here —
// this is a file the user has configured by hand and we are editing it for them.
const bobSettings = `{
  "workbench.colorTheme": "Default Dark Modern",
  "editor.fontSize": 13,
  "editor.fontFamily": "Menlo, Monaco, monospace",
  "editor.tabSize": 4,
  "editor.formatOnSave": true,
  "editor.rulers": [80, 100],
  "files.autoSave": "onFocusChange",
  "files.trimTrailingWhitespace": true,
  "terminal.integrated.fontSize": 12,
  "git.autofetch": true,
  "telemetry.telemetryLevel": "off",
  "http.proxyAuthorization": "keep-me",
  "extensions.autoUpdate": false
}`

// noPrompt stubs the confirmation for the duration of one test.
//
// Necessary, not tidiness: `go test` inherits the terminal it was launched from, so an
// unstubbed bobConfirm opens /dev/tty and blocks the run waiting for a keystroke.
// Returns the count of prompts so a test can assert one was actually asked.
func noPrompt(t *testing.T, answer bool) *int {
	t.Helper()
	asked := 0
	saved := bobConfirm
	bobConfirm = func(stdout io.Writer) bool {
		asked++
		return answer
	}
	t.Cleanup(func() { bobConfirm = saved })
	return &asked
}

// bobHalf builds the merged command around a synthetic launch, the way runBob
// assembles it, so a test can drive one verb or a sequence. The shell half's rc
// file is a separate scratch file, so assertions on the editor half's files
// watch those alone, and l.rcErr rides along as the shell half's by-hand advice
// the way runBob passes it.
func bobHalf(t *testing.T, l bobLaunch, settings, cfg string, yes bool, stdout, stderr io.Writer) bobCommand {
	t.Helper()
	home := t.TempDir()
	return bobCommand{
		home:          home,
		settingsPath:  settings,
		cortexCfgPath: cfg,
		launch:        l,
		shellRC:       filepath.Join(home, ".zshrc"),
		shellAdvise:   l.rcErr,
		yes:           yes,
		stdout:        stdout,
		stderr:        stderr,
	}
}

// bobDoc reads the settings file back as a flat map.
func bobDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	return doc
}

// The basic claim: the flat key lands, with the value derived from the config.
func TestBobEnable_WritesHTTPProxy(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := bobDoc(t, settings)[bobProxyKey]; got != "http://127.0.0.1:47600" {
		t.Errorf("%s = %v, want the config's proxy", bobProxyKey, got)
	}
	// Top-level and flat, not nested under anything — the VS Code shape.
	if _, nested := bobDoc(t, settings)["http"]; nested {
		t.Error(`wrote a nested "http" object; VS Code reads the flat dotted key`)
	}
}

// The property that matters most: this is the user's own editor configuration.
// bobHandFormatted is deliberately NOT what json.MarshalIndent would emit, in every
// way that matters:
//
//   - keys are NOT in alphabetical order (workbench first, editor before files)
//   - indentation is FOUR spaces, not two
//   - blank lines group related settings
//   - "editor.rulers" is an inline array on one line
//
// Every one of those is something a re-marshal destroys, and the fixture the other
// tests use cannot see it: bobSettings is two-space and close enough to sorted that a
// reformat is nearly invisible in it. A settings.json is hand-curated and frequently
// committed to a dotfiles repo, so this shape is the realistic one.
const bobHandFormatted = `{
    "workbench.colorTheme": "Default Dark Modern",

    "editor.rulers": [80, 120],
    "editor.fontSize": 13,
    "editor.tabSize": 4,

    "files.autoSave": "onFocusChange",
    "terminal.integrated.fontSize": 12
}
`

// The file must change by exactly the one line that adds the key — asserted on BYTES,
// not through a parse.
//
// This is the test TestBobEnable_PreservesEverythingElse cannot be. That one compares
// through bobDoc, which json.Unmarshals into a map[string]any, and a map has no key
// order and carries no whitespace — so every assertion it makes is blind to the entire
// class of damage a re-marshal does. It passed the whole time enable was alphabetizing
// the file, reindenting it from four spaces to two, and exploding inline arrays onto
// one line per element: on a real settings file that rewrote ten lines to add one key.
//
// So the assertion here is a line diff of the before and after bytes. Any reordering,
// any reindentation, any array reflow shows up as extra changed lines and fails.
func TestBobEnable_ChangesOneLineAndNoOtherByte(t *testing.T) {
	settings, cfg := fixture(t, bobHandFormatted)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	added, removed := lineDiff(string(before), string(after))
	got := string(after)

	// Adding a key to the last member's line also puts a comma on it, so the write
	// legitimately touches two lines: the new one, and the previous last one gaining
	// its comma. Nothing beyond that is this command's business.
	if len(added) != 2 || len(removed) != 1 {
		t.Errorf("want 1 line replaced by 2 (the comma and the new key), got -%d/+%d:\n--- removed\n%s\n+++ added\n%s",
			len(removed), len(added), strings.Join(removed, "\n"), strings.Join(added, "\n"))
	}
	for _, line := range added {
		if !strings.Contains(line, bobProxyKey) && !strings.Contains(line, "terminal.integrated.fontSize") {
			t.Errorf("an unrelated line was rewritten: %q", line)
		}
	}

	// The new line must be indented like its siblings, which is a separate claim from
	// "only one line was added" — an unindented new key is still exactly one new line,
	// so the count above cannot see it. It is worth its own assertion because it is a
	// mistake already made here once: reading the indentation from the line holding
	// the closing brace looks right and is wrong, because in a conventionally
	// formatted file that line is column 0.
	if !strings.Contains(got, "\n    \""+bobProxyKey+"\": ") {
		t.Errorf("the inserted key is not indented like its siblings:\n%s", got)
	}

	// The specific casualties of a re-marshal, each named so a regression reports
	// which property it broke rather than just "bytes differ".
	if !strings.Contains(got, `"editor.rulers": [80, 120]`) {
		t.Errorf("the inline array was reflowed:\n%s", got)
	}
	if !strings.Contains(got, "\n    \"editor.fontSize\": 13,") {
		t.Errorf("four-space indentation was not preserved:\n%s", got)
	}
	if strings.Index(got, "workbench.colorTheme") > strings.Index(got, "editor.fontSize") {
		t.Errorf("keys were alphabetized — workbench must still come first:\n%s", got)
	}
	if !strings.Contains(got, "\"Default Dark Modern\",\n\n    \"editor.rulers\"") {
		t.Errorf("a blank-line grouping separator was lost:\n%s", got)
	}
}

// enable then disable must return the file to the exact bytes it started with.
//
// A round trip through a re-marshal is stable — it reformats once and then agrees with
// itself — so this cannot be checked with the parsing helpers either. Byte equality is
// the whole claim: a user who enables and changes their mind gets their file back, not
// a reformatted equivalent of it.
func TestBobEnableDisable_RoundTripsByteForByte(t *testing.T) {
	settings, cfg := fixture(t, bobHandFormatted)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("enable: exit %d: %s", code, errb.String())
	}
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
		t.Fatalf("disable: exit %d: %s", code, errb.String())
	}

	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("round trip did not restore the file byte for byte:\n--- before\n%s\n+++ after\n%s", before, after)
	}
}

// Removing a key must not leave whitespace nobody wrote, whichever position it held.
//
// Each of these is a distinct splice: a middle member takes its own line and one comma,
// the LAST member has no following comma so it must take the PRECEDING one (a trailing
// comma is invalid JSON), the only member leaves an empty object, and a member between
// two blank-line separators must take one of them or the file gains a blank line.
//
// The SHARED-LINE rows are the ones the single-line row cannot stand in for, and the
// distinction is what a reviewer found broken. A whole-document single line is not
// ownLine on either side, so the splicer's "take the rest of the line" arm never fires
// for it. A member sharing ONE line of a multi-line document is the mixed case: it has
// a newline after it (so that arm did fire, welding the next line onto this one) and it
// may still be first on its line (so the leading-indent arm fired too, eating the
// indentation the surviving key needs). All three placements are pinned — the removed
// key first on the line, in the middle, and last — because each one exercises a
// different pair of those arms.
//
// The assertions are on bytes and on re-parseability, because "valid JSON" and "no
// stray whitespace" are different claims and the early implementations of this splicer
// satisfied the first while failing the second.
func TestBobDisable_LeavesNoStrayWhitespace(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "middle member",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\": \"http://127.0.0.1:47600\",\n    \"b\": 2\n}\n",
			want: "{\n    \"a\": 1,\n    \"b\": 2\n}\n",
		},
		{
			name: "last member takes the preceding comma",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\": \"http://127.0.0.1:47600\"\n}\n",
			want: "{\n    \"a\": 1\n}\n",
		},
		{
			name: "only member",
			in:   "{\n    \"http.proxy\": \"http://127.0.0.1:47600\"\n}\n",
			want: "{\n}\n",
		},
		{
			name: "between two grouping separators keeps exactly one",
			in:   "{\n    \"a\": 1,\n\n    \"http.proxy\": \"http://127.0.0.1:47600\",\n\n    \"b\": 2\n}\n",
			want: "{\n    \"a\": 1,\n\n    \"b\": 2\n}\n",
		},
		{
			name: "single line document",
			in:   "{\"a\": 1, \"http.proxy\": \"http://127.0.0.1:47600\", \"b\": 2}\n",
			want: "{\"a\": 1, \"b\": 2}\n",
		},
		{
			// Shares a line to its LEFT, next key on the line below. The reviewer's
			// case: the newline-taking arm used to fire here and produce
			// `"b": 2,     "c": 3` — two of the user's lines welded into one.
			name: "shares a line, next key on the following line",
			in:   "{\n    \"a\": 1,\n    \"b\": 2, \"http.proxy\": \"http://127.0.0.1:47600\",\n    \"c\": 3\n}\n",
			want: "{\n    \"a\": 1,\n    \"b\": 2,\n    \"c\": 3\n}\n",
		},
		{
			// Shares a line to its RIGHT. It IS first on its line, so the arm that
			// removes the line's leading indentation fired and left `{\n "b": 2,` —
			// the four-space indent replaced by one stray space.
			name: "shares a line, first on it",
			in:   "{\n    \"http.proxy\": \"http://127.0.0.1:47600\", \"b\": 2,\n    \"c\": 3\n}\n",
			want: "{\n    \"b\": 2,\n    \"c\": 3\n}\n",
		},
		{
			// Shares a line on BOTH sides, mid-document: neither line arm may fire.
			name: "shares a line on both sides",
			in:   "{\n    \"a\": 1,\n    \"b\": 2, \"http.proxy\": \"http://127.0.0.1:47600\", \"c\": 3\n}\n",
			want: "{\n    \"a\": 1,\n    \"b\": 2, \"c\": 3\n}\n",
		},
		{
			// Shares a line and is the document's last member, so there is no following
			// comma: the preceding-comma arm runs on a shared line.
			name: "shares a line and is last",
			in:   "{\n    \"a\": 1,\n    \"b\": 2, \"http.proxy\": \"http://127.0.0.1:47600\"\n}\n",
			want: "{\n    \"a\": 1,\n    \"b\": 2\n}\n",
		},
		{
			// The member STRADDLES a line break: valid JSON, and what VS Code's own
			// formatter produces when the value is long enough to wrap. This row is not
			// about whitespace — it is about not crashing. The splicer measured the
			// member's line end forward from the KEY, so the first newline it found was
			// the one inside the member, giving a line end BELOW the value's end and a
			// `slice bounds out of range [79:46]` panic. `configure bob disable` died on
			// a settings file that was never malformed, after printing that it was
			// keeping a .bak.
			name: "value on the line below its key",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\":\n        \"http://127.0.0.1:47600\",\n    \"b\": 2\n}\n",
			want: "{\n    \"a\": 1,\n    \"b\": 2\n}\n",
		},
		{
			// Straddling AND last, so the preceding-comma arm runs on a member whose
			// own span covers a newline. Separate row because the panic was in code
			// reached before that arm, so a crash fixed only for the following-comma
			// case would still show up here.
			name: "value on the line below its key, and last",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\":\n        \"http://127.0.0.1:47600\"\n}\n",
			want: "{\n    \"a\": 1\n}\n",
		},
		{
			// A space between the value and the newline, on a LAST member. The
			// preceding-comma arm walks the cut back over the comma above, which moves
			// it ABOVE this space — so the space belongs to a member that is gone and
			// lands on the line that survives: `"a": 1,` became `"a": 1 `, trailing
			// whitespace on a line the user never touched.
			//
			// Not the trigger the review reported, which was whitespace before the
			// COLON; the "whitespace everywhere a colon allows it" row below is that
			// layout, and it was already clean. The two are worth keeping apart, since
			// only one of them can reach this arm.
			name: "space between the value and the newline, last member",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\": \"http://127.0.0.1:47600\" \n}\n",
			want: "{\n    \"a\": 1\n}\n",
		},
		{
			// Same arm, tab instead of space: both are what an editor leaves behind, and
			// a fix written against ' ' alone would pass the row above and fail here.
			name: "tab between the value and the newline, last member",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\": \"http://127.0.0.1:47600\"\t\n}\n",
			want: "{\n    \"a\": 1\n}\n",
		},
		{
			// The bound on that absorption, in the opposite direction. Widening it to
			// every whitespace byte — isBobSpace, which includes '\n' — swallows the
			// newline as well and welds the closing brace onto the surviving line:
			// `"a": 1}`. Only the horizontal run, only as far as the line's end.
			name: "space then a blank line, last member",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\": \"http://127.0.0.1:47600\" \n\n}\n",
			want: "{\n    \"a\": 1\n\n}\n",
		},
		{
			// The layout the review named, pinned as already-correct rather than fixed:
			// whitespace before the colon is inside the member's own span, so the splice
			// takes it with the member and never strands anything. Kept so a future
			// change to this arm cannot break it silently.
			name: "whitespace before the colon",
			in:   "{\n    \"a\": 1,\n    \"http.proxy\"   : \"http://127.0.0.1:47600\"\n}\n",
			want: "{\n    \"a\": 1\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.in)

			var out, errb bytes.Buffer
			if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}

			got, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("bytes differ:\n got %q\nwant %q", got, tc.want)
			}
			// Belt and braces: a splice that produced the right bytes by accident but
			// broke the document would be caught here too.
			var doc map[string]any
			if jerr := json.Unmarshal(got, &doc); jerr != nil {
				t.Errorf("result is not valid JSON: %v\n%s", jerr, got)
			}
			if _, still := doc[bobProxyKey]; still {
				t.Errorf("the key survived:\n%s", got)
			}
		})
	}
}

// The offsets driving the splice come from json.Decoder, not from a text search, and
// this is the case that tells the two apart: the key's own name appears inside another
// member's string value, and as a nested object's key. A regex or strings.Index
// implementation edits the wrong one; a tokenizer cannot.
func TestBobDisable_IgnoresTheKeyNameInsideValuesAndNesting(t *testing.T) {
	in := "{\n" +
		"    \"some.note\": \"set \\\"http.proxy\\\": \\\"http://127.0.0.1:47600\\\" to use Cortex\",\n" +
		"    \"nested\": {\"http.proxy\": \"http://inner.example:1\"},\n" +
		"    \"http.proxy\": \"http://127.0.0.1:47600\",\n" +
		"    \"z\": 1\n}\n"

	settings, cfg := fixture(t, in)

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if jerr := json.Unmarshal(got, &doc); jerr != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", jerr, got)
	}

	if _, still := doc[bobProxyKey]; still {
		t.Errorf("the real top-level key survived:\n%s", got)
	}
	// The decoy in a string value must be untouched, character for character.
	if note, _ := doc["some.note"].(string); !strings.Contains(note, `"http.proxy"`) {
		t.Errorf("a mention of the key inside another value was edited: %q", note)
	}
	// The nested one is a different member of a different object.
	nested, _ := doc["nested"].(map[string]any)
	if nested[bobProxyKey] != "http://inner.example:1" {
		t.Errorf("a nested object's same-named key was edited: %v", nested)
	}
	if doc["z"] != float64(1) {
		t.Errorf("an unrelated key was lost: %v", doc["z"])
	}
}

// lineDiff reports which lines are only in b (added) and only in a (removed), counting
// duplicates. Good enough to assert "exactly these lines changed" without pulling in a
// diff library.
func lineDiff(a, b string) (added, removed []string) {
	count := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		count[l]++
	}
	for _, l := range strings.Split(b, "\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		added = append(added, l)
	}
	seen := map[string]int{}
	for _, l := range strings.Split(b, "\n") {
		seen[l]++
	}
	for _, l := range strings.Split(a, "\n") {
		if seen[l] > 0 {
			seen[l]--
			continue
		}
		removed = append(removed, l)
	}
	return added, removed
}

func TestBobEnable_PreservesEverythingElse(t *testing.T) {
	settings, cfg := fixture(t, bobSettings)
	before := bobDoc(t, settings)

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	after := bobDoc(t, settings)
	for k, want := range before {
		got, ok := after[k]
		if !ok {
			t.Errorf("key %q was dropped", k)
			continue
		}
		// Compare through JSON so numbers and arrays compare by value.
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		if string(wj) != string(gj) {
			t.Errorf("key %q changed: %s -> %s", k, wj, gj)
		}
	}
	// Exactly one key added, and it is ours. A neighbouring key that merely looks
	// related — http.proxyAuthorization — must be left alone, which the loop above
	// covers and this states.
	if len(after) != len(before)+1 {
		t.Errorf("key count %d -> %d, want exactly one added", len(before), len(after))
	}
	if after["http.proxyAuthorization"] != "keep-me" {
		t.Errorf("a neighbouring http.* key was touched: %v", after["http.proxyAuthorization"])
	}
	if _, err := os.Stat(settings + ".bak"); err != nil {
		t.Errorf("no backup written: %v", err)
	}
}

// Running enable twice must not prompt, rewrite, or fail the second time.
func TestBobEnable_IsIdempotent(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
	if code := c.enable(); code != 0 {
		t.Fatalf("first run: exit %d: %s", code, errb.String())
	}
	first, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	asked := noPrompt(t, false) // would decline; must not be consulted at all
	out.Reset()
	errb.Reset()
	c.yes = false
	if code := c.enable(); code != 0 {
		t.Fatalf("second run: exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second run does not report the existing state:\n%s", out.String())
	}
	if *asked != 0 {
		t.Errorf("prompted %d times for a no-op write", *asked)
	}
	second, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the file was rewritten by a run that had nothing to change")
	}
}

// A corporate proxy is someone's working network configuration. Refuse, name it, and
// change nothing — do not "fix" it.
func TestBobEnable_RefusesAForeignProxy(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://proxy.corp.example.com:3128"}`)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "proxy.corp.example.com:3128") {
		t.Errorf("the refusal does not name the value it refused to replace:\n%s", errb.String())
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused enable still wrote to the file")
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("a refused enable left a .bak, so it opened the file for writing")
	}
}

// Hardcoding 47600 and ~/.cortex/ca would point Bob at nothing the moment someone
// edited their config. The IPv6 form is the one that broke a sibling command: a
// strings.Cut on ":" produced http://[:1]:47600 from [::1]:47600.
func TestBobEnable_DerivesPortAndCAFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, addr, wantProxy string
	}{
		{"moved port", "127.0.0.1:19999", "http://127.0.0.1:19999"},
		{"ipv6 loopback", "[::1]:47655", "http://[::1]:47655"},
		{"wildcard host becomes loopback", "0.0.0.0:47600", "http://localhost:47600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, "{}")
			body, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			moved := strings.Replace(string(body), `"127.0.0.1:47600"`, `"`+tc.addr+`"`, 1)
			if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
				t.Fatal(err)
			}

			var out, errb bytes.Buffer
			if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			if got := bobDoc(t, settings)[bobProxyKey]; got != tc.wantProxy {
				t.Errorf("%s = %v, want %q", bobProxyKey, got, tc.wantProxy)
			}
			// The CA in the printed trust command comes from ca_dir, so a moved
			// config must not produce a command naming the default location.
			if strings.Contains(out.String(), filepath.Join(".cortex", "ca")) {
				t.Errorf("output names the default CA dir, not the config's:\n%s", out.String())
			}
		})
	}
}

// mode: disabled means nothing terminates TLS, so a written proxy is a broken editor.
// Same gate as claude-code and exec.
func TestBobEnable_RefusesADisabledBridge(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Replace(string(body), "mode: enabled", "mode: disabled", 1)
	if err := os.WriteFile(cfg, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 1 {
		t.Fatalf("exit = %d, want 1: %s", code, errb.String())
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("a refused enable opened the file for writing")
	}
}

// VS Code permits comments in settings.json; encoding/json does not. Refuse and say
// why — do not rewrite the user's file to strip them.
func TestBobEnable_RejectsCommentedSettings(t *testing.T) {
	settings, cfg := fixture(t, "{\n  // my theme\n  \"editor.fontSize\": 13\n}")
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "comment") {
		t.Errorf("the error does not name the likely cause:\n%s", errb.String())
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the commented file was rewritten")
	}
}

// Disable removes our value and leaves anyone else's alone. The asymmetry is the
// point: enable refuses a foreign value with exit 1, disable reports it with exit 0 —
// there is nothing wrong with a machine whose Bob uses a different proxy.
func TestBobDisable_RemovesOnlyOurs(t *testing.T) {

	t.Run("ours is removed", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13, "http.proxy": "http://127.0.0.1:47600"}`)
		var out, errb bytes.Buffer
		if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		doc := bobDoc(t, settings)
		if _, ok := doc[bobProxyKey]; ok {
			t.Errorf("%s survived disable", bobProxyKey)
		}
		if doc["editor.fontSize"] == nil {
			t.Error("an unrelated key was dropped")
		}
	})

	t.Run("a foreign proxy is left in place", func(t *testing.T) {
		settings, cfg := fixture(t, `{"http.proxy": "http://proxy.corp.example.com:3128"}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
			t.Fatalf("exit = %d, want 0 — a foreign proxy is not an error", code)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("disable removed a value it did not write")
		}
	})

	t.Run("the key being absent is not an error", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
		var out, errb bytes.Buffer
		if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "Not enabled") {
			t.Errorf("output does not say there was nothing to do:\n%s", out.String())
		}
	})

	t.Run("a non-string value is reported, not deleted", func(t *testing.T) {
		settings, cfg := fixture(t, `{"http.proxy": false}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a value agentop cannot have written was deleted anyway")
		}
	})
}

// The off switch must not depend on the thing being switched off. A user who has
// uninstalled Cortex and wants Bob working again must still be able to run disable.
//
// This is the test that caught the regression in the first version of the fix for
// the exact-match report. Judging ownership by comparing against the config is
// right when there IS a config; making that the only rule meant a missing config
// answered "not a Cortex proxy" for Cortex's own default address, and disable
// then refused to remove it. That is precisely the case where the off switch
// matters most, so bobOwns answers bobUnknown here — a loopback http proxy with
// nothing to compare against — and disable proceeds while saying what it is going
// on.
//
// Both halves are asserted, because removing the value is only half the contract:
// the user is entitled to know the judgment was made by shape rather than by
// matching their config.
//
// NARROWED to the PROMPTED path. This test used to pass yes=true, and the refusal
// added for the unattended case broke it — correctly. The two requirements collide
// only there: "the off switch must not depend on the config" (this test) and "a
// guess plus --yes is not consent"
// (TestBobDisable_RefusesToDeleteAGuessUnattended) are both satisfiable, because
// the safeguard --yes removes is the prompt that names the value. So the contract
// this test pins is unchanged in substance and now reads: with the config gone,
// disable still works — it asks first. Removing the prompt is what is refused, not
// removing the value.
func TestBobDisable_WithUnreadableConfig(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	want, ca := bobWanted(cfg, t.TempDir())
	// The point of the fixture: with the config gone there is nothing to compare
	// the value against, so ownership cannot be decided by matching.
	if want != "" {
		t.Fatalf("bobWanted invented a proxy address from a missing config: %q", want)
	}
	// The CA path is best-effort and must still produce something printable, or
	// the undo command disable prints would name nothing.
	if ca == "" {
		t.Fatal("bobWanted returned no CA path with the config gone")
	}
	if got := bobOwns("http://127.0.0.1:47600", want); got != bobUnknown {
		t.Errorf("bobOwns with no config = %v, want bobUnknown", got)
	}

	// Answers yes, standing in for a user who read the printed value and recognized
	// it. Asserting the prompt happened is what keeps this row distinct from the
	// unattended one: without it, a fix that dropped the prompt entirely would pass.
	asked := noPrompt(t, true)
	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).disable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if *asked != 1 {
		t.Errorf("prompted %d times, want 1", *asked)
	}
	if _, ok := bobDoc(t, settings)[bobProxyKey]; ok {
		t.Error("disable failed with the config missing, which is when it is most needed")
	}
	if !strings.Contains(out.String(), "shape alone") {
		t.Errorf("disable did not disclose that it judged by shape:\n%s", out.String())
	}
	// And WHICH file it could not read. "no readable config to compare against" was
	// the same sentence whether Cortex is uninstalled or --config was a typo, and only
	// the second is worth retrying — so the path is the part that tells them apart.
	// bobStatus's equivalent arm already named it; this one did not.
	if !strings.Contains(out.String(), cfg) {
		t.Errorf("disable did not name the config it could not read (%s):\n%s", cfg, out.String())
	}
}

// Status reports and never acts: three states, all exit 0, nothing written.
//
// The verdict is asserted as the WHOLE FIRST LINE, not as a substring anywhere in the
// output, because "leads with the verdict" is the property being claimed. This used to
// open with the raw `"http.proxy"=...` key and leave the reader to derive the answer,
// and a strings.Contains assertion cannot tell that shape from this one: it passes just
// as well with the verdict buried on line six. Equality on line one is what fails when
// something is prepended above it.
//
// The negative spelling is checked too, on the "ours" case. "IBM Bob is *NOT*
// configured..." CONTAINS "IBM Bob is", so a truncated or mis-assembled verdict could
// satisfy a prefix check while saying the opposite of the truth. The two strings are
// each other's trap, so each case pins that the other one is absent.
func TestBobStatus_ThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name, settings, want string
	}{
		{"absent", `{"editor.fontSize": 13}`, bobStatusNo},
		{"ours", `{"http.proxy": "http://127.0.0.1:47600"}`, bobStatusYes},
		{"foreign", `{"http.proxy": "http://proxy.corp.example.com:3128"}`, bobStatusNo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.settings)
			before, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}

			var out, errb bytes.Buffer
			if code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).status(); code != 0 {
				t.Fatalf("exit = %d, want 0 — a report is not a verdict", code)
			}

			lines := strings.Split(out.String(), "\n")
			if lines[0] != tc.want {
				t.Errorf("first line = %q, want %q\nfull output:\n%s", lines[0], tc.want, out.String())
			}
			// The other verdict must not appear anywhere: one report, one answer.
			other := bobStatusYes
			if tc.want == bobStatusYes {
				other = bobStatusNo
			}
			if strings.Contains(out.String(), other) {
				t.Errorf("both verdicts present:\n%s", out.String())
			}

			// The paragraph the message-simplification request asked to be removed. It
			// said status "does not act", which the exit code and this test's own
			// byte comparison below already establish.
			if strings.Contains(out.String(), "Not run here") {
				t.Errorf("the removed paragraph is back:\n%s", out.String())
			}

			after, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("status wrote to the settings file")
			}
			if _, err := os.Stat(settings + ".bak"); err == nil {
				t.Error("status left a .bak")
			}
		})
	}
}

// Liveness is a SEPARATE axis from ownership, and the WARNING line must track only the
// first. The distinction is not cosmetic: the naive version of this warned that a
// foreign proxy was down, which is both untrue (a corporate proxy is reachable from
// somewhere, just not from here) and none of agentop's business — it reads as a complaint
// about a setting this command deliberately leaves alone.
//
// Nothing listens on any of these ports in a test, so "nothing is listening" is the
// shared condition and the only variable is whose value it is. That is what makes the
// table a fair comparison: same liveness, different ownership, opposite expectations.
func TestBobStatus_WarnsOnlyForAProxyItClaims(t *testing.T) {
	// 47600 is NOT usable for the dead-proxy case: on a developer machine the real
	// Cortex proxy is listening on it, so the fixture's hardcoded port would make
	// "ours and nothing listening" quietly depend on whether the author had run
	// `agentop service stop`. It passed on CI and failed here, which is the wrong way
	// round for a test about liveness. So the whole table moves to a port the OS just
	// confirmed is free, in both the config and the settings value — ownership is a
	// whole host+port match, so the two must move together or the case stops being
	// about liveness at all.
	dead := freePort(t)

	for _, tc := range []struct {
		name, settings string
		wantWarning    bool
	}{
		// Ours and dead: the warning is the whole point — the setting is right and the
		// service is stopped, which is the normal state of a laptop.
		{"ours and nothing listening", `{"http.proxy": "http://127.0.0.1:` + dead + `"}`, true},
		// Not ours: silent. Judging someone else's proxy is out of scope.
		{"foreign", `{"http.proxy": "http://proxy.corp.example.com:3128"}`, false},
		// Drifted but still loopback: silent too. The detail lines already tell the
		// user how to replace the value (see TestBobStatus_ReportsPortDrift), so a
		// liveness complaint about the stale port on top of that is noise.
		{"drifted port", `{"http.proxy": "http://127.0.0.1:47699"}`, false},
		// Unset: there is no address to probe, so there is nothing to warn about.
		{"unset", `{"editor.fontSize": 13}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.settings)
			movePort(t, cfg, dead)

			var out, errb bytes.Buffer
			if code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).status(); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}

			got := out.String()
			warned := strings.Contains(got, "WARNING: No proxy is listening at ")
			if warned != tc.wantWarning {
				t.Errorf("warned = %v, want %v:\n%s", warned, tc.wantWarning, got)
			}
			if !tc.wantWarning {
				return
			}
			// When it does fire it must name the address, so the reader knows which
			// thing to start, and it must be the SECOND line — directly under the
			// verdict, above the detail — as the request specified.
			lines := strings.Split(got, "\n")
			if len(lines) < 2 || !strings.HasPrefix(lines[1], "WARNING: No proxy is listening at ") {
				t.Errorf("the warning is not on line 2:\n%s", got)
			}
			if !strings.Contains(lines[1], "127.0.0.1:"+dead) {
				t.Errorf("the warning does not name the address: %q", lines[1])
			}
		})
	}
}

// Status must distinguish "pointing at Cortex" from "pointing at the port Cortex uses
// now" — reporting drift is useful, silently moving it is not.
func TestBobStatus_ReportsPortDrift(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47699"}`)
	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).status(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "47699") || !strings.Contains(out.String(), "47600") {
		t.Errorf("drift report names neither the stale value nor the current one:\n%s", out.String())
	}

	// And the advice must be followable. This arm used to say "run `agentop configure
	// bob enable` to move it" full stop, which enable answers with exit 1: a drifted
	// PORT is bobNotOurs, and the arm that falls through to the write is a differing
	// spelling of the same address, not a different one. So status was sending the
	// user to a command that refuses.
	//
	// Pinned as behaviour rather than as wording: run enable on the very value status
	// just reported, and if it refuses, require that status did not offer a bare
	// `enable` as the way out. That keeps passing through a rewording and fails again
	// if either side moves — including if someone later lets enable claim a drifted
	// value, in which case the bare advice becomes true and this stops objecting.
	var enableOut, enableErr bytes.Buffer
	noPrompt(t, true) // enable must not block on a prompt if it gets that far
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &enableOut, &enableErr).enable(); code != 0 {
		// enable refuses, so the detail lines must not present it as the whole fix.
		// The bare sentence is the exact shape that was wrong.
		if strings.Contains(out.String(), "run `agentop configure bob enable` to move it") {
			t.Errorf("status sends the user to enable, which exits %d on that value:\nstatus:\n%s\nenable stderr:\n%s",
				code, out.String(), enableErr.String())
		}
		// Whatever it says instead has to name the value to move to, or the user is
		// left guessing which address is current.
		if !strings.Contains(out.String(), "by hand") && !strings.Contains(out.String(), "remove it") {
			t.Errorf("status offers no followable way to fix the drift:\n%s", out.String())
		}
	}
}

// A declined prompt is the user saying no, and it must cost nothing.
func TestBob_DeclinedPromptWritesNothing(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		asked := noPrompt(t, false)

		var out, errb bytes.Buffer
		c := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb)
		rcBefore, _ := os.ReadFile(c.shellRC)
		if code := c.enable(); code != exitDeclined {
			t.Fatalf("exit = %d, want %d", code, exitDeclined)
		}
		if *asked != 1 {
			t.Errorf("prompted %d times, want 1 — one prompt covers both halves", *asked)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a declined enable wrote to the settings file")
		}
		// One consent covers every write the preview listed, so refusing it
		// declines both halves: the rc file is as untouched as the settings.
		if rcAfter, _ := os.ReadFile(c.shellRC); !bytes.Equal(rcBefore, rcAfter) {
			t.Error("a declined enable wrote to the rc file")
		}
	})

	t.Run("disable", func(t *testing.T) {
		settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		noPrompt(t, false)

		var out, errb bytes.Buffer
		if code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).disable(); code != exitDeclined {
			t.Fatalf("exit = %d, want %d", code, exitDeclined)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a declined disable wrote to the file")
		}
	})
}

// A stray operand is usually a mistyped flag, and silently ignoring it means the run
// did something other than what was asked.
func TestBob_RejectsStrayArguments(t *testing.T) {
	for _, verb := range []string{"enable", "disable", "status"} {
		t.Run(verb, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBob([]string{verb, "extra"}, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), `"extra"`) {
				t.Errorf("the error does not quote the stray argument: %s", errb.String())
			}
		})
	}
}

// The printed command is the deliverable of the trust step, and it is copy-pasted by
// hand. An unquoted path with a space splits into two arguments: the settings write
// succeeds and the command under it silently does not, which is the worse of the two
// failures.
func TestBob_TrustMessageQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "Application Support", "ca.crt")
	for name, got := range map[string]string{
		"trust":         bobTrustNote(spaced),
		"untrust macOS": bobUntrustNote(spaced, true),
		"untrust other": bobUntrustNote(spaced, false),
	} {
		if strings.Contains(got, spaced) && !strings.Contains(got, shellQuote(spaced)) {
			t.Errorf("%s note contains the bare path, unquoted:\n%s", name, got)
		}
	}
}

// The request named bundle.crt; this implementation deliberately prints ca.crt.
//
// bundle.crt holds the bridge CA plus every platform root, and exists only for tools
// whose CA setting REPLACES the trust store. The macOS keychain is additive, so
// `add-trusted-cert -r trustRoot` on the bundle would install machine-wide explicit
// root trust for every public CA in it — far broader than asked, and one
// `delete-certificate -c authbridge-tls-bridge-ca` would not reverse it. This pins the
// deviation so it cannot be "corrected" back to the request's wording.
func TestBob_TrustMessageNamesCaCrtNotBundle(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.crt")
	for name, got := range map[string]string{
		"untrust": bobUntrustNote(ca, true),
	} {
		for _, line := range strings.Split(got, "\n") {
			if !strings.Contains(line, "security ") {
				continue // prose may mention bundle.crt to explain the choice
			}
			if strings.Contains(line, "bundle.crt") {
				t.Errorf("%s note runs security against the whole-trust-store bundle:\n%s", name, line)
			}
		}
	}
	// And the undo command has to name the keychain it added to, or a System-keychain
	// add is not undone by it.
	if !strings.Contains(bobUntrustNote(ca, true), bobSystemKeychain) {
		t.Errorf("the untrust command does not name the keychain:\n%s", bobUntrustNote(ca, true))
	}
}

// Guessing ~/.config/IBM Bob/... is the VS Code convention but unverified for this
// app, and writing a proxy setting into a file nothing reads is a silent no-op. Refuse
// and name the flag that works instead.
func TestBob_SettingsPathNonDarwin(t *testing.T) {
	got, err := bobSettingsPath("/home/someone")
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("darwin should know the path: %v", err)
		}
		if !strings.Contains(got, filepath.Join("IBM Bob", "User", "settings.json")) {
			t.Errorf("path = %q", got)
		}
		return
	}
	if err == nil {
		t.Fatalf("guessed a path on %s: %q", runtime.GOOS, got)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("the error does not name the platform it does not know: %v", err)
	}
}

// The ownership test decides what disable deletes, so its false positives cost more
// than claude-code's isCortexValue (a strings.Contains, which feeds a refusal there).
//
// This table replaces TestBobIsCortexProxy, whose predicate was a bool over a port
// PREFIX. Two things were wrong with it and one test cannot have caught both:
//
//   - "476" as a prefix is not the 476xx block it documented. It also accepted
//     http://127.0.0.1:476 and http://127.0.0.1:4769999, and it accepted 47600 on a
//     machine whose Cortex listens on 19999 — the reported bug.
//   - A bool has no room for "cannot tell". With the config gone there is nothing to
//     compare against, and answering false there broke disable for the uninstalled
//     case. bobUnknown is that third answer.
//
// So the cases are grouped by which of the two they pin, and every one names the
// wantProxy it is judged against — the parameter the old predicate did not have.
func TestBobOwns(t *testing.T) {
	const want = "http://127.0.0.1:19999" // deliberately NOT in the 476xx block

	for _, tc := range []struct {
		name, val, wantProxy string
		owns                 bobOwnership
	}{
		// Judged against a config: equality on host and port, nothing looser.
		{"exact match", "http://127.0.0.1:19999", want, bobOurs},
		{"surrounding space", "  http://127.0.0.1:19999  ", want, bobOurs},
		// wantedFromLoaded itself rewrites an empty/0.0.0.0/:: host, so the two
		// loopback spellings have to be one identity or enable and disable disagree.
		{"localhost for 127.0.0.1", "http://localhost:19999", want, bobOurs},
		{"127.0.0.1 for localhost", "http://127.0.0.1:19999", "http://localhost:19999", bobOurs},
		{"ipv6 loopback", "http://[::1]:19999", want, bobOurs},

		// The reported bug. Every one of these passed the old prefix test while the
		// configured port was 19999, so disable would have deleted them.
		{"the bug: 476 prefix", "http://127.0.0.1:47600", want, bobNotOurs},
		{"the bug: bare 476", "http://127.0.0.1:476", want, bobNotOurs},
		{"the bug: 4769999", "http://127.0.0.1:4769999", want, bobNotOurs},

		{"empty", "", want, bobNotOurs},
		{"corporate proxy", "http://proxy.corp.example.com:3128", want, bobNotOurs},
		{"loopback, wrong port", "http://127.0.0.1:3128", want, bobNotOurs},
		{"right port, not loopback", "http://192.168.1.5:19999", want, bobNotOurs},

		// A substring match says yes to both of these. That is why this parses.
		{"port in a query string", "http://corp.example.com/?next=127.0.0.1:19999", want, bobNotOurs},
		{"suffixed host", "http://localhost:19999.evil.example.com", want, bobNotOurs},

		// enable only ever writes http://, so anything else is not ours to remove.
		{"https", "https://127.0.0.1:19999", want, bobNotOurs},
		{"socks5", "socks5://127.0.0.1:19999", want, bobNotOurs},

		// Everything outside host and port. url.Parse files these under User, Path,
		// RawQuery and Fragment, and comparing only Hostname()/Port() reads none of
		// them — so each of these matched the configured proxy exactly and disable
		// deleted it. The credentials row is the worst of them: the secret goes out
		// with the value.
		//
		// These are NOT the substring rows above. Those fail a whole-value comparison
		// on the host; these all carry the right host and the right port, and are
		// still somebody else's setting.
		{"credentials", "http://user:secret@127.0.0.1:19999", want, bobNotOurs},
		{"username only", "http://user@127.0.0.1:19999", want, bobNotOurs},
		{"a PAC script path", "http://127.0.0.1:19999/proxy.pac", want, bobNotOurs},
		{"a query", "http://127.0.0.1:19999/?next=evil", want, bobNotOurs},
		{"a query with no path", "http://127.0.0.1:19999?next=evil", want, bobNotOurs},
		{"a fragment", "http://127.0.0.1:19999#frag", want, bobNotOurs},

		// The other edge of that gate, and the reason it tests Path against a set
		// rather than for emptiness: url.Parse gives "" for http://h:p and "/" for
		// http://h:p/, so treating any non-empty Path as foreign would make a
		// hand-typed trailing slash unremovable. enable writes the first form; a
		// user who typed the second still wrote our address.
		{"trailing slash", "http://127.0.0.1:19999/", want, bobOurs},

		// Same gate on the no-config path, which reaches it through a different
		// branch: there is no value to compare against, so only the shape decides.
		{"no config, credentials", "http://user:secret@127.0.0.1:47600", "", bobNotOurs},
		{"no config, a path", "http://127.0.0.1:47600/proxy.pac", "", bobNotOurs},
		{"no config, trailing slash", "http://127.0.0.1:47600/", "", bobUnknown},

		// No config to compare against: the third state. A loopback http proxy is
		// the shape agentop writes, so it is removable; anything else is not.
		{"no config, loopback", "http://127.0.0.1:47600", "", bobUnknown},
		{"no config, localhost", "http://localhost:1234", "", bobUnknown},
		{"no config, ipv6", "http://[::1]:47600", "", bobUnknown},
		{"no config, corporate", "http://proxy.corp.example.com:3128", "", bobNotOurs},
		{"no config, routable ip", "http://192.168.1.5:47600", "", bobNotOurs},
		{"no config, https", "https://127.0.0.1:47600", "", bobNotOurs},
		{"no config, empty value", "", "", bobNotOurs},

		// An unparseable wantProxy must degrade to the no-config answer, not to a
		// match: comparing against a host that failed to parse would make every
		// port-less value equal to it.
		{"unparseable config value", "http://127.0.0.1:47600", "::::", bobUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bobOwns(tc.val, tc.wantProxy); got != tc.owns {
				t.Errorf("bobOwns(%q, %q) = %v, want %v", tc.val, tc.wantProxy, got, tc.owns)
			}
		})
	}
}

// A settings file may legally name the same key twice, and every JSON reader that
// keeps one value keeps the LAST — Go's encoding/json does, and so does VS Code. That
// makes a duplicate invisible to every decision this command makes: readSettings
// decodes into a map, so the first value is gone before anything looks at it.
//
// The reported failure was disable. It spliced out the FIRST occurrence, left the
// second in place, and printed "Disabled. IBM Bob no longer routes through Cortex."
// while Bob was still routed through it — a false statement with exit 0 behind it.
//
// So all three verbs refuse instead, and all three are asserted here rather than just
// the one that was reported: they share bobDuplicateKey, and a regression in it would
// surface in whichever verb the next reader happens to run.
//
// The file is compared byte-for-byte after each run because "refuses" has to mean the
// file is untouched, not merely that the exit code changed.
func TestBob_RefusesADuplicateProxyKey(t *testing.T) {
	// Both occurrences point at Cortex, so nothing here turns on ownership: the
	// refusal is about not being able to say which member is the setting. A file
	// where only one of them is ours would let a reader think the check is an
	// ownership check.
	const dup = `{
    "editor.fontSize": 13,
    "http.proxy": "http://127.0.0.1:47600",
    "http.proxyStrictSSL": false,
    "http.proxy": "http://127.0.0.1:47600"
}`

	// The premise. If encoding/json ever started rejecting a duplicate key, or kept
	// the first value, the rest of this test would be reasoning about a file shape
	// that cannot occur — so the shape is verified before it is relied on.
	t.Run("the premise: a decoder hides the duplicate", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(dup), &doc); err != nil {
			t.Fatalf("the fixture is not valid JSON, so the whole test is vacuous: %v", err)
		}
		if len(doc) != 3 {
			t.Errorf("decoded %d keys, want 3 — a duplicate should collapse", len(doc))
		}
	})

	for _, verb := range []string{"enable", "disable", "status"} {
		t.Run(verb, func(t *testing.T) {
			settings, cfg := fixture(t, dup)
			before, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
			var code int
			switch verb {
			case "enable":
				code = c.enable()
			case "disable":
				code = c.disable()
			case "status":
				code = c.status()
			}

			// status reports; the two writers fail. Both are "refused" — the
			// difference is only whether refusing is this verb's answer or its error.
			if verb == "status" {
				if code != 0 {
					t.Errorf("exit = %d, want 0 — a report is not a verdict", code)
				}
				if lines := strings.Split(out.String(), "\n"); lines[0] != bobStatusNo {
					t.Errorf("first line = %q, want %q\n%s", lines[0], bobStatusNo, out.String())
				}
				// Reporting either value as "the setting" would be the guess this
				// check exists to avoid.
				if strings.Contains(out.String(), bobStatusYes) {
					t.Errorf("status claimed the configuration it cannot read:\n%s", out.String())
				}
			} else if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}

			// The key must be NAMED. "this file has a problem" sends the reader
			// looking; the key name and the word duplicate tell them what to delete.
			said := out.String() + errb.String()
			for _, want := range []string{strconv.Quote(bobProxyKey), "more than once"} {
				if !strings.Contains(said, want) {
					t.Errorf("output does not contain %q:\n%s", want, said)
				}
			}

			// The reported symptom, asserted directly so a regression names itself:
			// disable claimed success over a file it had not fixed.
			if strings.Contains(said, "no longer routes through Cortex") {
				t.Errorf("%s claimed the proxy was removed:\n%s", verb, said)
			}

			after, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("%s wrote to a file it refused:\n--- before\n%s\n--- after\n%s",
					verb, before, after)
			}
			if _, err := os.Stat(settings + ".bak"); err == nil {
				t.Errorf("%s left a .bak behind for a write it did not make", verb)
			}
		})
	}
}

// A duplicate somewhere OTHER than the proxy key is none of this check's business.
// Refusing on any duplicate at all would make the command unusable on a settings file
// whose unrelated keys happen to repeat, and the splice only ever touches one member.
func TestBob_ADuplicateOfAnotherKeyIsNotRefused(t *testing.T) {
	settings, cfg := fixture(t, `{"editor.fontSize": 13, "editor.fontSize": 14}`)
	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errb.String())
	}
	if got := bobDoc(t, settings)[bobProxyKey]; got == nil {
		t.Errorf("enable refused a file whose duplicate is not its key:\n%s%s", out.String(), errb.String())
	}
}

// The table above pins bobOwns; this pins that disable actually consults it, on the
// one case where getting it wrong destroys something the user cannot recover.
//
// A proxy URL carrying credentials is not a hypothetical shape — it is how an
// authenticating forward proxy is configured in a settings file, and the password is
// usually nowhere else. The old ownership test compared only Hostname() and Port(), so
// `http://user:secret@127.0.0.1:47600` on a machine whose Cortex listens on 47600 was
// judged ours and deleted, secret included.
//
// Written through bobDisable rather than as another bobOwns row because the claim is
// about the file: a correct verdict that the caller ignores loses the value just the
// same. The secret is asserted present in the bytes, not merely the key.
func TestBobDisable_LeavesACredentialBearingProxyAlone(t *testing.T) {
	const secret = "s3cr3t-not-ours"
	settings, cfg := fixture(t,
		`{"http.proxy": "http://corpuser:`+secret+`@127.0.0.1:47600"}`)

	// The port is Cortex's own, and the host is loopback — everything the old check
	// looked at says this is ours. Only the userinfo says otherwise, which is the
	// point.
	_, _ = bobWanted(cfg, t.TempDir()) // the ownership premise, checked by TestBobOwns

	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable(); code != 0 {
		t.Fatalf("exit = %d, want 0 — a foreign proxy is not an error: %s", code, errb.String())
	}

	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("disable rewrote a file it does not own:\n--- before\n%s\n--- after\n%s", before, after)
	}
	if !bytes.Contains(after, []byte(secret)) {
		t.Error("the credential is gone from the settings file")
	}
	// And it must say so: silently leaving the value would look identical to having
	// removed it, which is the report the user acts on.
	if !strings.Contains(out.String(), "http.proxy") {
		t.Errorf("disable did not name the value it left in place:\n%s", out.String())
	}
}

// The three states must be distinct values, or a switch over them collapses two
// cases into one and every table above still passes.
func TestBobOwnershipStatesAreDistinct(t *testing.T) {
	if bobNotOurs == bobOurs || bobOurs == bobUnknown || bobNotOurs == bobUnknown {
		t.Fatalf("two states share a value: notOurs=%d ours=%d unknown=%d",
			bobNotOurs, bobOurs, bobUnknown)
	}
	// bobNotOurs must be the zero value: a bobOwnership that was never assigned
	// has to mean "do not touch it", never "delete it".
	var zero bobOwnership
	if zero != bobNotOurs {
		t.Errorf("the zero bobOwnership is %d, not bobNotOurs — an unset value would authorise a delete", zero)
	}
}

// Report #4's named gap: no test round-tripped enable then disable on a port outside
// the 476xx block, which is exactly the pair the two halves of the old suite
// contradicted each other about. TestBobEnable_DerivesPortAndCAFromConfig asserted
// enable WRITES http://127.0.0.1:19999, TestBobIsCortexProxy asserted that shape is
// not ours, and nothing crossed between them — so a 50-test suite passed over a
// disable that could not undo its own enable.
//
// Written as a round trip rather than as two assertions about one port, because what
// broke was the relationship between the two commands and not either one alone.
func TestBob_EnableDisableRoundTripOnANon476xxPort(t *testing.T) {
	const port = "19999"

	settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:"+port, 1)
	if moved == string(body) {
		t.Fatal("the fixture config no longer names 127.0.0.1:47600, so this test moved nothing")
	}
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	var enOut, enErr bytes.Buffer
	c := bobHalf(t, bobLaunch{}, settings, cfg, true, &enOut, &enErr)
	if code := c.enable(); code != 0 {
		t.Fatalf("enable exit %d: %s", code, enErr.String())
	}
	got, _ := bobDoc(t, settings)[bobProxyKey].(string)
	if want := "http://127.0.0.1:" + port; got != want {
		t.Fatalf("enable wrote %q, want %q", got, want)
	}

	// The half that was missing. Same config, so disable judges the value against
	// the address enable derived it from.
	var disOut, disErr bytes.Buffer
	c.stdout, c.stderr = &disOut, &disErr
	if code := c.disable(); code != 0 {
		t.Fatalf("disable exit %d: %s", code, disErr.String())
	}
	if _, ok := bobDoc(t, settings)[bobProxyKey]; ok {
		t.Errorf("disable could not undo its own enable on port %s:\n%s", port, disOut.String())
	}
	// The failure mode being pinned was a silent refusal, not a crash: disable
	// exited 0 and said the value was not Cortex's. Assert the refusal wording is
	// absent, so a regression cannot pass by keeping the exit code.
	if strings.Contains(disOut.String(), "not a Cortex proxy") {
		t.Errorf("disable called its own enable's value foreign:\n%s", disOut.String())
	}
	// An unrelated key must survive both writes.
	if bobDoc(t, settings)["editor.fontSize"] == nil {
		t.Error("a sibling key did not survive the round trip")
	}
}

// bobSameLoopback is what makes localhost and 127.0.0.1 one identity, and it must do
// that WITHOUT resolving names: a DNS lookup in this path would make ownership depend
// on the resolver, and a machine whose "localhost" resolves elsewhere would get a
// different answer for the same settings file.
func TestBobSameLoopback(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"127.0.0.1", "127.0.0.1", true},
		{"localhost", "127.0.0.1", true},
		{"127.0.0.1", "localhost", true},
		{"::1", "localhost", true},
		{"localhost", "::1", true},

		{"127.0.0.1", "192.168.1.5", false},
		{"localhost", "proxy.corp.example.com", false},
		// Not a loopback spelling agentop recognises, so it is only equal to itself.
		{"127.0.0.2", "127.0.0.1", false},
		{"127.0.0.2", "127.0.0.2", true},
		{"", "", true},
		{"", "127.0.0.1", false},
	} {
		if got := bobSameLoopback(tc.a, tc.b); got != tc.same {
			t.Errorf("bobSameLoopback(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// Liveness is a separate axis from ownership, and conflating them is a bug in both
// directions: a stopped Cortex must not make its own value foreign, and a stranger's
// proxy that happens to be up must not become ours.
func TestBobProxyIsListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := "http://" + ln.Addr().String()

	if !bobProxyIsListening(live) {
		t.Errorf("a listening socket reported as not listening: %s", live)
	}

	// Close it and ask again: the same URL, the opposite answer. A predicate that
	// ignored its argument would pass the first assertion alone.
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if bobProxyIsListening("http://" + addr) {
		t.Errorf("a closed socket reported as listening: %s", addr)
	}

	// Unparseable and empty values must answer false rather than panicking — this
	// runs on whatever is in the user's settings file.
	for _, bad := range []string{"", "::::", "not a url", "http://"} {
		if bobProxyIsListening(bad) {
			t.Errorf("bobProxyIsListening(%q) = true", bad)
		}
	}

	// Ownership must not move when liveness does. Nothing is listening on this
	// port now, and the value is still ours.
	if got := bobOwns("http://"+addr, "http://"+addr); got != bobOurs {
		t.Errorf("a stopped proxy changed ownership: got %v, want bobOurs", got)
	}
}

// The printed undo must actually undo the change earlier versions of enable printed.
// Those printed `add-trusted-cert -d -r trustRoot`, which writes trust settings to the
// ADMIN domain, and disable once printed only `delete-certificate -t`, whose own usage
// text says it removes the certificate and "user trust settings" — a different domain.
// So a user who ran both was left with admin-domain trustRoot settings for a
// certificate that no longer existed, and no printed command had removed them.
//
// enable no longer prints the add on macOS — the launch environment replaced it — but
// people who ran it still need the undo, so the undo is pinned against the exact
// command those versions printed.
func TestBobUntrustNoteUndoesTheEarlierKeychainAdd(t *testing.T) {
	ca := "/tmp/ca.crt"
	const earlierAdd = "sudo security add-trusted-cert -d -r trustRoot -k " + bobSystemKeychain + " /tmp/ca.crt"
	untrust := bobUntrustNote(ca, true)

	// The undo for exactly that, with -d repeated. Without the -d, remove-trusted-cert
	// works on the user domain, which the add never touched.
	if !strings.Contains(earlierAdd, "add-trusted-cert -d") || !strings.Contains(untrust, "remove-trusted-cert -d") {
		t.Errorf("the undo does not remove admin-domain trust settings:\n%s", untrust)
	}
	// And the certificate itself, from the keychain the add named. A delete that
	// defaults to the login keychain does not undo a System-keychain add.
	if !strings.Contains(untrust, "delete-certificate") || !strings.Contains(untrust, bobSystemKeychain) {
		t.Errorf("the undo does not delete the certificate from %s:\n%s", bobSystemKeychain, untrust)
	}
	// Conditional, because this version of enable never asked for the add.
	if !strings.Contains(untrust, "if an earlier agentop") {
		t.Errorf("the undo reads as though this enable made a keychain change:\n%s", untrust)
	}
}

// The backup promise must match what writeSettings actually does. It writes <path>.bak
// from the file's current contents only when the file EXISTS and no .bak is there
// already — so a single unconditional "a copy is kept as <path>.bak" was false twice:
// once for a settings file agentop is creating, and once on a second run, where the
// existing .bak is deliberately not overwritten and therefore holds the file as first
// found rather than as it is now.
//
// Three branches, three different true statements. Each asserts its own wording AND
// the absence of the claim that would be wrong there, because a message that says
// everything says nothing.
func TestBobBackupNote_MatchesWhatWriteSettingsDoes(t *testing.T) {
	// home is the settings file's directory, so the note's DISPLAY path is the
	// tilde abbreviation while the stat sees the real one — both spellings the
	// merged preview hands this function in production.
	note := func(t *testing.T, path string) string {
		t.Helper()
		return bobBackupNote(filepath.Dir(path), path)
	}

	t.Run("no existing file: nothing to promise", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		got := note(t, path)
		// Nothing at all: the preview already shows the path enable creates, and a
		// "no backup" line for a file that does not exist is noise before the one
		// question that matters.
		if got != "" {
			t.Errorf("says something about a backup of a file that does not exist: %q", got)
		}
	})

	t.Run("file exists, no .bak yet: a copy is kept", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := note(t, path)
		if !strings.Contains(got, "kept as ~/settings.json.bak") {
			t.Errorf("does not promise the backup it will make:\n%s", got)
		}
		if strings.Contains(got, "No backup is made") || strings.Contains(got, "already exists") {
			t.Errorf("claims a different branch's outcome:\n%s", got)
		}
	})

	t.Run("a .bak already exists: it is not overwritten", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".bak", []byte(`{"original": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := note(t, path)
		if !strings.Contains(got, "already exists") {
			t.Errorf("does not say the existing backup is left alone:\n%s", got)
		}
		// The distinction that matters to someone deciding whether to trust the
		// .bak: it holds the file as FIRST found, not as it is now.
		if !strings.Contains(got, "as first found") {
			t.Errorf("does not say which state the existing backup holds:\n%s", got)
		}
	})

	// The note is a claim about writeSettings, so check it against writeSettings
	// rather than only against itself. A message and an implementation that drift
	// apart is the whole bug.
	t.Run("the claim matches the behaviour", func(t *testing.T) {
		for _, exists := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "settings.json")
			if exists {
				if err := os.WriteFile(path, []byte(`{"pre": 1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			promised := strings.Contains(note(t, path), "copy of the original is kept")
			if err := writeSettings(path, map[string]any{"http.proxy": "http://127.0.0.1:47600"}); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path + ".bak")
			made := statErr == nil
			if promised != made {
				t.Errorf("file existed = %v: note promised a copy = %v, writeSettings made one = %v",
					exists, promised, made)
			}
		}
	})
}

// freePort returns a TCP port on loopback that nothing is listening on, by binding one
// and closing it immediately.
//
// Inherently a race — the port could be taken between the close and the probe — but a
// far smaller one than hardcoding 47600, which on a developer machine is occupied by
// the very service under discussion, deterministically and for the whole session. An
// ephemeral port the kernel just handed out is not reused that quickly.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// movePort rewrites the fixture config's forward-proxy port, so a test can choose an
// address instead of inheriting 47600 from the fixture. Same strings.Replace trick
// TestClaudeCodeEnable_ReadsAddressesFromConfig uses on the same fixture.
func movePort(t *testing.T, cfgPath, port string) {
	t.Helper()
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:"+port, 1)
	if moved == string(body) {
		t.Fatalf("the fixture config no longer names 127.0.0.1:47600, so the port could not be moved:\n%s", body)
	}
	if err := os.WriteFile(cfgPath, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --yes must not delete a proxy that is only a GUESS.
//
// The reviewer's fixture, executed literally before anything was reasoned about:
// `disable --yes --config /nonexistent` against `{"http.proxy":
// "http://localhost:3128"}` silently removed Squid's proxy and exited 0.
//
// The mechanism is worth stating precisely, because the PR body first understated
// it. With no readable config, bobWanted returns proxy == "" — so bobOwns has no
// address to compare against and falls back to judging SHAPE alone, which makes the
// verdict bobUnknown. Shape is "http, loopback host, no userinfo/path/query": that
// is EVERY loopback http proxy on ANY port, not just Cortex's 476xx block, because
// with no config there is no port in hand to compare. Squid on 3128 satisfies it.
//
// Interactively that is survivable — bobConfirm prints the value and the user
// recognizes their own proxy. --yes is precisely the removal of that safeguard, so
// the two together are the unsafe combination and the one this refuses.
//
// The three rows after the first are the regression half: each is a path that MUST
// still work, and each differs from the refusing row in exactly one respect (the
// flag, the config, or the value). Without them a fix that simply stopped deleting
// would pass.
func TestBobDisable_RefusesToDeleteAGuessUnattended(t *testing.T) {
	// Not Cortex's port, and not in the 476xx block at all — the whole point is that
	// the port is irrelevant when there is no config to compare it against.
	const foreign = "http://localhost:3128"

	// Subtest names deliberately carry NO flag spellings. t.TempDir() names its
	// directory after the subtest, bobDisable interpolates the settings path into its
	// message, and an assertion that the message mentions "--yes" then matches the
	// PATH instead of the prose. That is not hypothetical: it is what a mutation
	// removing "--yes" from the message survived on, until these rows were renamed.
	for _, tc := range []struct {
		name     string
		settings string
		// readableConfig false means --config pointed at nothing, which is what makes
		// bobOwns fall back to shape.
		readableConfig bool
		yes            bool
		wantCode       int
		wantRemoved    bool
	}{
		// The bug. A guess plus --yes is not consent.
		{"guess, unattended", foreign, false, true, 1, false},
		// Same guess, same value, WITHOUT --yes: the prompt is the safeguard, so this
		// path must still reach it. noPrompt answers yes below, standing in for a user
		// who looked at the printed value and recognized it as Cortex's.
		{"guess, prompted", foreign, false, false, 0, true},
		// --yes with a readable config and a matching value: ownership is known, not
		// guessed, so the refusal must not touch this. This is the scripted path the
		// feature exists for.
		{"known ours, unattended", `http://127.0.0.1:47600`, true, true, 0, true},
		// --yes with a readable config and a foreign value: bobNotOurs, which was
		// already left alone at exit 0 and must stay that way. Exit 0, not 1 — nothing
		// is wrong with this file.
		{"known foreign, unattended", foreign, true, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, `{"http.proxy": "`+tc.settings+`", "editor.fontSize": 13}`)

			// The config path is the only lever that decides known-vs-guessed, so it is
			// what the table varies. An unreadable path is how a user who has
			// uninstalled Cortex, or mistyped --config, arrives here.
			cfgArg := cfg
			if !tc.readableConfig {
				cfgArg = filepath.Join(t.TempDir(), "does-not-exist.yaml")
			}
			want, _ := bobWanted(cfgArg, t.TempDir())

			// Asserted rather than assumed: if bobWanted ever started guessing a proxy
			// address, every "guess" row would silently become a "known" row and this
			// test would keep passing while testing nothing.
			if tc.readableConfig && want == "" {
				t.Fatalf("the fixture config did not yield a proxy address, so this row cannot be about known ownership")
			}
			if !tc.readableConfig && want != "" {
				t.Fatalf("an unreadable config yielded proxy %q, so this row cannot be about a guess", want)
			}

			before, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}

			asked := noPrompt(t, true)
			var out, errb bytes.Buffer
			code := bobHalf(t, bobLaunch{}, settings, cfgArg, tc.yes, &out, &errb).disable()

			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, out.String(), errb.String())
			}

			after, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			_, stillSet := bobDoc(t, settings)[bobProxyKey]
			if removed := !stillSet; removed != tc.wantRemoved {
				t.Errorf("removed = %v, want %v\n--- before\n%s\n--- after\n%s",
					removed, tc.wantRemoved, before, after)
			}

			// A sibling key, to catch a fix that "left the file alone" by rewriting it
			// wholesale. Present in every row, removed in none.
			if _, ok := bobDoc(t, settings)[`editor.fontSize`]; !ok {
				t.Errorf("an unrelated key was lost:\n%s", after)
			}

			if !tc.wantRemoved {
				// Byte-identical, not merely still-parsing-the-same: a refusal that
				// reformatted the file would have written where it said it would not.
				if !bytes.Equal(before, after) {
					t.Errorf("the file was rewritten despite no removal:\n--- before\n%s\n--- after\n%s", before, after)
				}
				// writeSettings makes a .bak on its first write, so its absence is
				// independent evidence that nothing was written — the assertion above
				// would still pass if the file were rewritten to identical bytes.
				if _, err := os.Stat(settings + ".bak"); err == nil {
					t.Error("a .bak was created, so something was written")
				}
			}

			if tc.wantCode == 1 {
				// The refusal must be on stderr, and ONLY there: exit 1 with the
				// explanation on stdout is unreadable in the scripted use this exists
				// to protect, and a copy on both streams double-prints it for anyone
				// merging them.
				//
				// Asserted as absence-from-stdout rather than as `errb.Len() != 0`.
				// A length check here is SUBSUMED: every state that empties errb also
				// fails the three content assertions below, which all read errb — a
				// mutation copying the refusal to stdout while leaving stderr intact
				// survived all four, which is how this assertion got its present
				// shape.
				if strings.Contains(out.String(), "cannot be confirmed as one") {
					t.Errorf("the refusal is on stdout too: %q", out.String())
				}
				// Naming the value is what lets the user tell "my Squid" from "stale
				// Cortex" — a bare refusal sends them to the file to find out.
				if !strings.Contains(errb.String(), tc.settings) {
					t.Errorf("the refusal does not name the value it declined to delete: %q", errb.String())
				}
				// Naming the config path, for the same reason the interactive caveat
				// does: "the Cortex config could not be read" does not say which file,
				// so a typo'd --config and an uninstalled Cortex read identically.
				// Only rows that got here through an unreadable config can assert it.
				if !tc.readableConfig && !strings.Contains(errb.String(), cfgArg) {
					t.Errorf("the refusal does not name the config it could not read (%s): %q", cfgArg, errb.String())
				}
				// Naming the way out is the difference between a refusal and a dead
				// end. Both routes are asserted: drop --yes, or supply a config.
				for _, want := range []string{"--yes", "--config"} {
					if !strings.Contains(errb.String(), want) {
						t.Errorf("the refusal does not mention %s: %q", want, errb.String())
					}
				}
				// And it must not have asked: the refusal replaces the prompt under
				// --yes, it does not precede one.
				if *asked != 0 {
					t.Errorf("prompted %d times under --yes", *asked)
				}
			}

			// The prompted row is the one that proves the safeguard still exists. If
			// the refusal had been written to fire regardless of --yes, this would be 0.
			if tc.name == "guess, prompted" && *asked != 1 {
				t.Errorf("prompted %d times, want 1 — the guess must still be offered interactively", *asked)
			}
		})
	}
}

// bobSetKey has two ways to write a key: splice a new member in, or replace the span of
// one already there. Every other enable test takes the insert path, because a settings
// file that already names http.proxy is either identical to what we would write (so
// enable stops at "Already enabled") or foreign (so it refuses). Replacing needs the
// narrow middle: ours, and spelled differently.
//
// "localhost:47600" in the file against the "127.0.0.1:47600" the config derives is
// exactly that — bobOwns calls it ours, the strings differ, so enable falls through to
// a write over an existing member. Reached by no other test in this file: replacing the
// branch body with panic() leaves the whole suite green.
func TestBobEnable_ReplacesADifferentSpellingOfTheSameListener(t *testing.T) {
	settings, cfg := fixture(t, `{
  "editor.fontSize": 13,
  "http.proxy": "http://localhost:47600",
  "http.proxyAuthorization": "keep-me"
}`)
	noPrompt(t, true)

	var out, errb bytes.Buffer
	if code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// Not "Already enabled": the values differ, so this must be a write.
	if strings.Contains(out.String(), "Already enabled") {
		t.Errorf("treated a differing spelling as already correct:\n%s", out.String())
	}

	doc := bobDoc(t, settings)
	if got := doc[bobProxyKey]; got != "http://127.0.0.1:47600" {
		t.Errorf("%s = %v, want the config's spelling", bobProxyKey, got)
	}
	// The whole point of replacing rather than inserting: one member, not two. A
	// splice that appended instead would leave the key named twice, which bobFindMember
	// then refuses outright on the next run — so this also pins that enable stays
	// re-runnable.
	if n := strings.Count(mustRead(t, settings), `"http.proxy"`); n != 1 {
		t.Errorf("the key appears %d times, want 1", n)
	}
	// Its neighbours must survive, and so must its position: a replace that dropped
	// the surrounding spans would take these with it.
	for _, want := range []string{`"editor.fontSize": 13`, `"http.proxyAuthorization": "keep-me"`} {
		if !strings.Contains(mustRead(t, settings), want) {
			t.Errorf("lost %s:\n%s", want, mustRead(t, settings))
		}
	}
}

// mustRead returns the file's bytes as a string, for the assertions that are about
// layout rather than about the parsed document.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A missing settings.json is not a missing Bob. IBM Bob writes the file the first time a
// setting changes, so a Bob that has run on every default has none — and enable refused
// exactly that user with "IBM Bob has not saved settings here". What says Bob has run is
// its User directory, which it creates on first launch. Each shape below pins what the
// three verbs do with it, on a fresh fixture per verb so one verb's write cannot become
// the next one's input.
//
// Subtest names deliberately carry no flag spelling. bobNotInstalled interpolates the
// path into its message, t.TempDir() names its directory after the subtest, so a name
// containing "--settings" would make an assertion on that string match the path
// instead of the prose — which has already silently defeated one mutation in this
// file's history.
func TestBobVerbs_SettingsDocumentShapes(t *testing.T) {
	type expect struct {
		code   int
		has    []string
		hasNot []string
	}
	for _, tc := range []struct {
		name string
		// setup returns the settings path to use, given a fixture's.
		setup                   func(t *testing.T, settings string) string
		enable, disable, status expect
		// created: enable leaves a settings file holding the proxy.
		created bool
	}{
		{
			name:    "absent in Bob's directory",
			setup:   func(t *testing.T, settings string) string { return settings },
			created: true,
			enable:  expect{code: 0, has: []string{"Nothing else in the files changes"}},
			disable: expect{code: 0, has: []string{"does not exist; nothing to remove there", "Nothing to do"}},
			status:  expect{code: 0, has: []string{bobStatusNo, "does not exist yet"}, hasNot: []string{bobStatusUnknown}},
		},
		{
			name: "empty",
			setup: func(t *testing.T, settings string) string {
				if err := os.WriteFile(settings, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return settings
			},
			created: true,
			enable:  expect{code: 0},
			disable: expect{code: 0, has: []string{"sets no", "Nothing to do"}},
			status:  expect{code: 0, has: []string{bobStatusNo}, hasNot: []string{bobStatusUnknown, "does not exist yet"}},
		},
		{
			name: "no Bob directory",
			setup: func(t *testing.T, settings string) string {
				return filepath.Join(filepath.Dir(settings), "IBM Bob", "User", "settings.json")
			},
			enable:  expect{code: 1, has: []string{"has not run here", "Nothing to enable", "Start IBM Bob", "--settings"}},
			disable: expect{code: 0, has: []string{"has not run here; nothing to remove there", "Nothing to do"}},
			status:  expect{code: 0, has: []string{bobStatusUnknown}, hasNot: []string{bobStatusYes, bobStatusNo}},
		},
		{
			name: "only the null literal",
			setup: func(t *testing.T, settings string) string {
				if err := os.WriteFile(settings, []byte("null\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return settings
			},
			enable:  expect{code: 1, has: []string{"holds only `null`", "Nothing to enable"}},
			disable: expect{code: 0, has: []string{"holds only `null`", "Nothing to do"}},
			status:  expect{code: 0, has: []string{bobStatusUnknown}, hasNot: []string{bobStatusYes, bobStatusNo}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(t *testing.T, got int, text string, e expect) {
				t.Helper()
				if got != e.code {
					t.Errorf("exit = %d, want %d\n%s", got, e.code, text)
				}
				for _, w := range e.has {
					if !strings.Contains(text, w) {
						t.Errorf("output omits %q:\n%s", w, text)
					}
				}
				for _, w := range e.hasNot {
					if strings.Contains(text, w) {
						t.Errorf("output has %q:\n%s", w, text)
					}
				}
			}

			t.Run("enable", func(t *testing.T) {
				base, cfg := fixture(t, "")
				settings := tc.setup(t, base)
				asked := noPrompt(t, true)
				before, beforeErr := os.ReadFile(settings)
				var out, errb bytes.Buffer
				code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).enable()
				check(t, code, out.String()+errb.String(), tc.enable)
				if !tc.created {
					// A refusal: no prompt, nothing written, no directory conjured up.
					if *asked != 0 {
						t.Errorf("prompted %d times before refusing", *asked)
					}
					after, afterErr := os.ReadFile(settings)
					if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
						t.Error("changed the settings file for a refusal")
					}
					if _, err := os.Stat(filepath.Dir(settings)); beforeErr != nil && err == nil {
						t.Error("created Bob's directory for a refusal")
					}
					if _, err := os.Stat(settings + ".bak"); err == nil {
						t.Error("wrote a .bak for a refusal")
					}
					return
				}
				if got := bobDoc(t, settings)[bobProxyKey]; got != "http://127.0.0.1:47600" {
					t.Errorf("%s = %v after enable, want the config's proxy", bobProxyKey, got)
				}
			})
			t.Run("disable", func(t *testing.T) {
				base, cfg := fixture(t, "")
				settings := tc.setup(t, base)
				noPrompt(t, true)
				before, beforeErr := os.ReadFile(settings)
				var out, errb bytes.Buffer
				code := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb).disable()
				check(t, code, out.String()+errb.String(), tc.disable)
				after, afterErr := os.ReadFile(settings)
				if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
					t.Errorf("disable changed a file holding nothing of agentop's")
				}
			})
			t.Run("status", func(t *testing.T) {
				base, cfg := fixture(t, "")
				settings := tc.setup(t, base)
				var out, errb bytes.Buffer
				code := bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).status()
				check(t, code, out.String()+errb.String(), tc.status)
			})
		})
	}
}

// The two shapes status cannot judge must not be reported identically. Their being
// indistinguishable is what made the old behaviour hard to see: a --settings typo and a
// null document produced the same output, so a typo looked like a working run against
// an unconfigured Bob.
func TestBobStatus_DistinguishesWhyThereIsNoDocument(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct {
		name, body string
		nodir      bool
	}{
		{name: "no Bob directory", nodir: true},
		{name: "only the null literal", body: "null\n"},
	} {
		settings, cfg := fixture(t, "")
		if tc.nodir {
			settings = filepath.Join(filepath.Dir(settings), "missing", "settings.json")
		} else if err := os.WriteFile(settings, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		bobHalf(t, bobLaunch{}, settings, cfg, false, &out, &errb).status()
		// The path varies per subtest, so compare only the reason's prose, with
		// the paths that name it stripped out.
		reason := ""
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(line, "  editor: ") {
				reason = strings.TrimPrefix(strings.TrimPrefix(line, "  editor: "), settings)
				reason = strings.TrimSpace(strings.TrimPrefix(reason, filepath.Dir(settings)))
			}
		}
		if reason == "" {
			t.Fatalf("%s: no editor line in:\n%s", tc.name, out.String())
		}
		if prev, dup := seen[reason]; dup {
			t.Errorf("%s reports identically to %s: %q", tc.name, prev, reason)
		}
		seen[reason] = tc.name
	}
}

// The merged command configures BOTH faces: the editor's settings.json and the
// rc-file shell function. Each half's own behaviour is pinned by its own tests
// (here for the editor, cmd_bobshell_test.go for the shell); what only the merged
// command can lose is that it reaches both, so that is what this pins.
func TestBob_EnableConfiguresEditorAndShellBoth(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	settings, cfg := fixture(t, `{"editor.fontSize": 13}`)

	var out, errb bytes.Buffer
	code := runBob([]string{"enable", "--yes", "--settings", settings, "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d: %s%s", code, out.String(), errb.String())
	}
	if got := bobDoc(t, settings)[bobProxyKey]; got != "http://127.0.0.1:47600" {
		t.Errorf("editor half: %s = %v, want the config's proxy", bobProxyKey, got)
	}
	rc := filepath.Join(home, ".zshrc")
	b, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("shell half: no rc file at %s: %v", rc, err)
	}
	if !strings.Contains(string(b), bobShellBlock) {
		t.Errorf("shell half: %s holds no bobshell block:\n%s", rc, b)
	}
}

// One consent covers every write the preview listed, so a declined prompt
// declines BOTH halves: the user saying no to the list has not said yes to half
// of it. The command exits 3, because none of its changes happened — a script
// branching on the code must not read it as done.
//
// Until 2026-10-16 each half prompted separately and a refusal in one left the
// other applied; folding the prompts is what made the refusal whole.
func TestBob_DeclinedPromptDeclinesBothHalves(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	settings, cfg := fixture(t, `{"editor.fontSize": 13}`)

	asked := noPrompt(t, false)
	var out, errb bytes.Buffer
	code := runBob([]string{"enable", "--settings", settings, "--config", cfg}, &out, &errb)
	if code != exitDeclined {
		t.Fatalf("exit = %d, want %d — the changes were declined", code, exitDeclined)
	}
	if *asked != 1 {
		t.Errorf("prompted %d times, want 1 — one prompt covers both halves", *asked)
	}
	if bobDoc(t, settings)[bobProxyKey] != nil {
		t.Error("editor half: a declined prompt still wrote the key")
	}
	b, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err == nil {
		t.Errorf("shell half: a declined prompt still wrote the rc file:\n%s", b)
	}
}

// The shell half's findings are not conditional on the other half having nothing
// to do. A bobshell block agentop declines to touch (hand-edited, duplicated) is
// a note set exactly when the half will NOT write — so it can never ride the
// shellChanges arm, which requires a write. With editor-half changes pending,
// that used to leave the note unprinted and the headline claiming both halves,
// and a disable could say "IBM Bob no longer routes through Cortex" while bob()
// was still in the rc file. The note is a finding, not a failure — base's own
// command exited 0 on it — so the exit code stays 0; only the claim had to
// narrow to the half that acted.
func TestBob_ShellFindingsSurviveEditorChangesPending(t *testing.T) {
	t.Run("enable: an edited block is named, the headline claims only the editor half", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
		var out, errb bytes.Buffer
		c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
		edited := "\n" + bobShellMarkerStart + "\n# hand-edited since agentop wrote it\n" + bobShellMarkerEnd + "\n"
		if err := os.WriteFile(c.shellRC, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := c.enable(); code != 0 {
			t.Fatalf("exit = %d, want 0 — a left-alone block is a finding, not a failure: %s%s",
				code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "already has a cortex bobshell block") {
			t.Errorf("the shell half's finding is not shown:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Enabled the editor half.") {
			t.Errorf("the headline does not say which half was enabled:\n%s", out.String())
		}
		if strings.Contains(out.String(), "\nEnabled.\n") {
			t.Errorf("the headline claims the shell half too:\n%s", out.String())
		}
		// The edited block must still be there, untouched.
		got, err := os.ReadFile(c.shellRC)
		if err != nil || string(got) != edited {
			t.Errorf("the rc file changed:\n%s", got)
		}
	})

	t.Run("disable: duplicated blocks are named, the headline claims only the editor half", func(t *testing.T) {
		settings, cfg := fixture(t, `{"`+bobProxyKey+`": "http://127.0.0.1:47600"}`)
		var out, errb bytes.Buffer
		c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
		doubled := strings.Repeat(bobShellBlock, 2)
		if err := os.WriteFile(c.shellRC, []byte(doubled), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := c.disable(); code != 0 {
			t.Fatalf("exit = %d, want 0 — a left-alone block is a finding, not a failure: %s%s",
				code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "has 2 copies of the cortex bobshell block") {
			t.Errorf("the shell half's finding is not shown:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Disabled the editor half.") {
			t.Errorf("the headline does not say which half was disabled:\n%s", out.String())
		}
		if strings.Contains(out.String(), "no longer routes through Cortex") {
			t.Errorf("the headline claims the shell half too — bob() is still in the file:\n%s", out.String())
		}
	})
}

// The other side of the findings rule: a half whose requested state already
// holds has nothing to report. Disable's plain-absence notes — "Not enabled in
// X. Nothing to do." — say the shell half's work is already done, so with
// editor-half changes pending they are not findings: printing "Nothing to do."
// above an Apply? prompt contradicts the preview, and narrowing the headline to
// "Disabled the editor half." takes a full, successful disable and describes it
// as half of one. The note still prints in the nothing-to-do arm, where the
// whole command has nothing to change.
func TestBob_ShellAbsenceIsNotAFinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string // "" means no rc file at all
	}{
		{name: "rc file holds nothing of ours", content: "export EDITOR=vim\n"},
		{name: "no rc file at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, `{"`+bobProxyKey+`": "http://127.0.0.1:47600"}`)
			var out, errb bytes.Buffer
			c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
			if tc.content != "" {
				if err := os.WriteFile(c.shellRC, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if code := c.disable(); code != 0 {
				t.Fatalf("exit = %d, want 0 — the disable succeeded outright: %s%s",
					code, out.String(), errb.String())
			}
			if strings.Contains(out.String(), "Nothing to do.") {
				t.Errorf("the shell half's plain absence is reported as a finding:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "Disabled — IBM Bob no longer routes through Cortex.") {
				t.Errorf("a full disable is not reported as one:\n%s", out.String())
			}
		})
	}

	// The absence note itself is not gone — it belongs in the nothing-to-do arm,
	// where the whole command has nothing to change. Asserted on the rc-naming
	// form, because the arm's closing line ("Not enabled. Nothing to do.")
	// satisfies a looser "Not enabled" on its own.
	t.Run("the note still prints when nothing at all is to do", func(t *testing.T) {
		settings, cfg := fixture(t, `{}`)
		var out, errb bytes.Buffer
		c := bobHalf(t, bobLaunch{}, settings, cfg, true, &out, &errb)
		if err := os.WriteFile(c.shellRC, []byte("export EDITOR=vim\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := c.disable(); code != 0 {
			t.Fatalf("exit = %d: %s%s", code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "Not enabled in "+c.shellRC) {
			t.Errorf("the shell half's absence note is not reported:\n%s", out.String())
		}
	})
}

// A skipped editor half is 1, like bobNotInstalled: the half's change did not
// happen, and bobCombine must not let the shell half's success read as the whole
// command's. Base set the code in runBob's skip arm; the merge moved the skip
// into bobCommand and dropped it, so off macOS — the only place the skip fires —
// enable and disable exited 0 with a headline claiming more than ran.
func TestBob_SkippedEditorHalfExitsOne(t *testing.T) {
	_, cfg := fixture(t, "")
	var out, errb bytes.Buffer
	c := bobHalf(t, bobLaunch{}, "", cfg, true, &out, &errb)
	c.ideSkip = true
	if code := c.enable(); code != 1 {
		t.Fatalf("enable exit = %d, want 1 — the editor half's change did not happen:\n%s%s",
			code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Enabled the shell half.") {
		t.Errorf("the headline does not say which half was enabled:\n%s", out.String())
	}
	if strings.Contains(out.String(), "\nEnabled.\n") {
		t.Errorf("the headline claims the editor half too:\n%s", out.String())
	}

	// The same rule on the off switch, where a script branching on the code is
	// likeliest: disable's rc file holds exactly one block, so the shell half
	// has real work and succeeds — and the code still carries the skip's 1.
	var out2, errb2 bytes.Buffer
	c2 := bobHalf(t, bobLaunch{}, "", cfg, true, &out2, &errb2)
	c2.ideSkip = true
	if err := os.WriteFile(c2.shellRC, []byte(bobShellBlock), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := c2.disable(); code != 1 {
		t.Fatalf("disable exit = %d, want 1 — the editor half's change did not happen:\n%s%s",
			code, out2.String(), errb2.String())
	}
	if !strings.Contains(out2.String(), "Disabled the shell half.") {
		t.Errorf("the headline does not say which half was disabled:\n%s", out2.String())
	}
}

// The backup promise must survive the trip through the merged preview. The note
// stats the file it is asked about, so handing it the tilde-abbreviated DISPLAY
// path — the spelling every other line of the report uses — made the stat fail
// on every default macOS layout, where settings.json lives under $HOME: no
// promise on the first write, and no warning when a .bak from an earlier run is
// the copy that survives. Driving bobBackupNote directly cannot see this; the
// settings file has to sit under the command's home, the way it does for real.
func TestBob_EnablePromisesTheBackupItWillMake(t *testing.T) {
	run := func(t *testing.T, withBak bool) string {
		t.Helper()
		home := t.TempDir()
		settings := filepath.Join(home, "settings.json")
		if err := os.WriteFile(settings, []byte(`{"editor.fontSize": 13}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if withBak {
			if err := os.WriteFile(settings+".bak", []byte(`{"original": true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, cfg := fixture(t, "")
		var out, errb bytes.Buffer
		c := bobCommand{
			home:          home,
			settingsPath:  settings,
			cortexCfgPath: cfg,
			launch:        bobLaunch{},
			shellRC:       filepath.Join(home, ".zshrc"),
			yes:           true,
			stdout:        &out,
			stderr:        &errb,
		}
		if code := c.enable(); code != 0 {
			t.Fatalf("exit = %d: %s%s", code, out.String(), errb.String())
		}
		return out.String()
	}

	t.Run("first write: a copy is kept", func(t *testing.T) {
		got := run(t, false)
		if !strings.Contains(got, "A copy of the original is kept as ~/settings.json.bak") {
			t.Errorf("no promise of the backup this write makes:\n%s", got)
		}
	})

	t.Run("a .bak already exists: it is the one that survives", func(t *testing.T) {
		got := run(t, true)
		if !strings.Contains(got, "already exists and holds the file as first found") {
			t.Errorf("does not say which state the surviving backup holds:\n%s", got)
		}
		if strings.Contains(got, "A copy of the original is kept") {
			t.Errorf("promises a fresh copy it will not make:\n%s", got)
		}
	})
}

// The exit-code combination, as a table: the WORSE of the two halves wins, and a
// failure outranks a declined prompt, which outranks success.
func TestBobCombine(t *testing.T) {
	for _, tc := range []struct {
		ide, shell, want int
	}{
		{0, 0, 0},
		{0, exitDeclined, exitDeclined},
		{exitDeclined, 0, exitDeclined},
		{1, 0, 1},
		{0, 1, 1},
		{1, exitDeclined, 1},
		{exitDeclined, 1, 1},
		{exitDeclined, exitDeclined, exitDeclined},
		{2, 0, 2},
		{0, 2, 2},
	} {
		if got := bobCombine(tc.ide, tc.shell); got != tc.want {
			t.Errorf("bobCombine(%d, %d) = %d, want %d", tc.ide, tc.shell, got, tc.want)
		}
	}
}
