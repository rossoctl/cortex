package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// treeText renders data with fs and returns the lines as plain text.
func treeText(t *testing.T, data string, fs foldState) []string {
	t.Helper()
	root, err := treeFromJSON([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range renderTree(root, fs) {
		out = append(out, stripANSI(l.text))
	}
	return out
}

func lineWith(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// The plugin detail pane and searchdoc_test read this format. It must not change for JSON
// that has no multi-line string.
func TestRenderTree_AllOpenKeepsTheOldFormat(t *testing.T) {
	got := stripANSI(ColorizeJSONBytes([]byte(`{"b":{"c":"x"},"a":1,"d":[],"e":[true,null]}`)))
	want := `{
  "a": 1,
  "b": {
    "c": "x"
  },
  "d": [],
  "e": [
    true,
    null
  ]
}`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTree_MultiLineStringIsABlock(t *testing.T) {
	got := treeText(t, `{"s":"one\ntwo"}`, foldState{all: true})
	want := []string{"{", `  "s": │`, "    one", "    two", "}"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A block has no quotes, so it must escape what strconv.Quote used to escape. Rendered
// without forceColour, so any ESC in the output came from the content.
func TestRenderTree_EscapesControlRunes(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"s": "a\x1b[31mred\nb\tc"})
	out := strings.Join(treeText(t, string(data), foldState{all: true}), "\n")
	if strings.Contains(out, "\x1b") {
		t.Errorf("a raw ESC reached the output: %q", out)
	}
	if !strings.Contains(out, `a\x1b[31mred`) || !strings.Contains(out, "b    c") {
		t.Errorf("want the ESC escaped and the tab as four spaces: %q", out)
	}
}

func TestRenderTree_ClipsAtFiveLines(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"s": "1\n2\n3\n4\n5\n6\n7\n8\n9\n10"})
	got := treeText(t, string(data), foldState{})
	if lineWith(got, "    6") >= 0 {
		t.Errorf("line 6 shows on a clipped string: %q", got)
	}
	if lineWith(got, "    5") < 0 || lineWith(got, "… 5 more lines") < 0 {
		t.Errorf("want lines 1-5 and a '… 5 more lines' head: %q", got)
	}
	open := treeText(t, string(data), foldState{open: map[string]foldChoice{"s": {open: true}}})
	if lineWith(open, "    10") < 0 || lineWith(open, "more") >= 0 {
		t.Errorf("an open string must show every line and no more-line: %q", open)
	}
}

func TestRenderTree_ClipsAt1KB(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"s": strings.Repeat("x", 3000)})
	got := treeText(t, string(data), foldState{})
	if i := lineWith(got, "xxx"); i < 0 || strings.Count(got[i], "x") != clipBytes {
		t.Errorf("want one line of %d x: %q", clipBytes, got)
	}
	if lineWith(got, "… 2.0kB more") < 0 {
		t.Errorf("want '… 2.0kB more': %q", got)
	}
}

func TestRenderTree_ClipKeepsRunesWhole(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"s": strings.Repeat("é", 600)})
	for _, l := range treeText(t, string(data), foldState{}) {
		if !utf8.ValidString(l) {
			t.Errorf("the clip split a rune: %q", l)
		}
	}
}

func TestRenderTree_DefaultFolds(t *testing.T) {
	big := strings.Repeat("p", 2000)
	data := `{"inference":{"model":"m",
		"tools":[{"name":"Read","description":"reads"}],
		"toolResults":[{"toolUseId":"t1","content":"ok"}],
		"toolCalls":[{"name":"Read"}]},
		"mcp":{"params":{"q":"` + big + `"},"result":{"r":1}},
		"a2a":{"parts":[{"content":"hi"}]}}`
	got := treeText(t, data, foldState{})
	for _, want := range []string{`▸ "tools": [1]`, `▸ "toolResults": [1]`, `▸ "toolCalls": [1]`, `▸ "params": {1}`} {
		if lineWith(got, want) < 0 {
			t.Errorf("want a closed head %q: %q", want, got)
		}
	}
	for _, open := range []string{`"r": 1`, `"content": "hi"`, `"model": "m"`} {
		if lineWith(got, open) < 0 {
			t.Errorf("want %q open (small or not a default fold): %q", open, got)
		}
	}
	tools := treeText(t, data, foldState{open: map[string]foldChoice{"inference.tools": {open: true}}})
	if lineWith(tools, "▸ Read · ") < 0 || lineWith(tools, "reads") >= 0 {
		t.Errorf("an opened tool list shows each tool closed: %q", tools)
	}
}

func TestRenderTree_EarlierMessagesGroup(t *testing.T) {
	data := `{"inference":{"messages":[
		{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},{"role":"assistant","content":"a2"},
		{"role":"user","content":"u3"}]}}`
	got := treeText(t, data, foldState{})
	if lineWith(got, "▸ 3 earlier messages · ") < 0 {
		t.Fatalf("want a closed group of 3: %q", got)
	}
	if lineWith(got, `"u2"`) >= 0 {
		t.Errorf("an earlier message shows inside a closed group: %q", got)
	}
	for _, want := range []string{`"a2"`, `"u3"`} {
		if lineWith(got, want) < 0 {
			t.Errorf("want %s open after the group: %q", want, got)
		}
	}
	open := treeText(t, data, foldState{open: map[string]foldChoice{"inference.messages[<3]": {open: true}}})
	if lineWith(open, "▸ user · ") < 0 || lineWith(open, "▸ assistant · ") < 0 {
		t.Errorf("an open group shows each message closed, with its role: %q", open)
	}
}

func TestRenderTree_NoGroupOnAFirstTurn(t *testing.T) {
	got := treeText(t, `{"inference":{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}}`, foldState{})
	if lineWith(got, "earlier") >= 0 || lineWith(got, `"u"`) < 0 {
		t.Errorf("a first turn has no group and shows every message: %q", got)
	}
}

func TestRenderTree_LastMessageIsAssistant(t *testing.T) {
	got := treeText(t, `{"inference":{"messages":[{"role":"user","content":"u"},{"role":"assistant","content":"a"}]}}`, foldState{})
	if lineWith(got, "▸ 1 earlier message · ") < 0 || lineWith(got, `"a"`) < 0 {
		t.Errorf("want a group of 1 and the assistant message open: %q", got)
	}
}

// A search opens every fold that holds a match.
func TestRenderTree_QueryOpensTheFoldHoldingAMatch(t *testing.T) {
	data := `{"inference":{"tools":[{"name":"Read","description":"has a needle"}]}}`
	got := treeText(t, data, foldState{query: "needle"})
	if lineWith(got, "needle") < 0 {
		t.Errorf("the match is not rendered: %q", got)
	}
	closed := treeText(t, data, foldState{query: "needle", open: map[string]foldChoice{"inference.tools": {query: "needle"}}})
	if lineWith(closed, "needle") >= 0 {
		t.Errorf("a fold the reader closed opened for a search: %q", closed)
	}
}

// A new query opens a fold the reader closed only when the fold holds a match.
func TestRenderTree_NewQueryKeepsAClosedFoldWithNoMatch(t *testing.T) {
	data := `{"a":{"x":"needle"},"b":{"y":"other"}}`
	closed := map[string]foldChoice{"a": {}, "b": {}}
	got := treeText(t, data, foldState{query: "needle", open: closed})
	if lineWith(got, `"x": "needle"`) < 0 {
		t.Errorf("a closed fold that holds the match did not open: %q", got)
	}
	if lineWith(got, `"y": "other"`) >= 0 {
		t.Errorf("a closed fold with no match opened: %q", got)
	}
}

func TestRenderTree_LabelsAreEscaped(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"inference": map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "\x1b[2Jboom"},
		map[string]any{"role": "assistant", "content": "a"},
	}}})
	open := treeText(t, string(data), foldState{open: map[string]foldChoice{"inference.messages[<1]": {open: true}}})
	out := strings.Join(open, "\n")
	if strings.Contains(out, "\x1b") {
		t.Errorf("a raw ESC reached a label: %q", out)
	}
	if lineWith(open, `▸ user · `) < 0 || lineWith(open, `\x1b[2Jboom`) < 0 {
		t.Errorf("want the label with the ESC escaped: %q", open)
	}
}

func TestRenderTree_HeadLinesCarryTheirPath(t *testing.T) {
	root, err := treeFromJSON([]byte(`{"inference":{"tools":[{"name":"Read"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range renderTree(root, foldState{}) {
		if strings.Contains(l.text, `"tools"`) {
			if l.path != "inference.tools" || !l.head || l.open {
				t.Errorf("tools head = %+v, want path inference.tools, head, closed", l)
			}
			return
		}
	}
	t.Fatal("no tools line")
}

func TestParentPath(t *testing.T) {
	for in, want := range map[string]string{
		"inference.messages[<3][1].content": "inference.messages[<3][1]",
		"inference.messages[<3][1]":         "inference.messages[<3]",
		"inference.messages[<3]":            "inference.messages",
		"inference":                         "",
		"":                                  "",
		"a]":                                "",
		"mcp.params.a]":                     "mcp.params",
	} {
		if got := parentPath(in); got != want {
			t.Errorf("parentPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// toolDoc is v as JSON, so a test can write a JSON string that holds JSON without escapes.
func toolDoc(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A tool call's arguments and a tool's one-line JSON output show as their value, at the paths
// where the detail view puts them, one level only. The same key elsewhere, a string inside a
// decoded value, and output with line breaks stay strings.
func TestRenderTree_ToolArgumentsAndOutputDecodeOneLevel(t *testing.T) {
	doc := toolDoc(t, map[string]any{
		"inference": map[string]any{"toolCalls": []any{map[string]any{
			"arguments": `{"command":"cd x\ncat y","arguments":"{\"a\":1}"}`, "other": `{"a":1}`}},
			"toolResults": []any{map[string]any{"content": `[1,{"content":"{\"b\":2}"}]`}, map[string]any{"content": "{\n\"a\": 1}"}},
			"arguments":   `{"a":1}`},
		"mcp": map[string]any{"params": map[string]any{"arguments": `{"a":1}`}},
	})
	want := []string{"{", `  "inference": {`, `    "arguments": "{\"a\":1}",`, `    "toolCalls": [`, "      {",
		`        "arguments": {`, `          "arguments": "{\"a\":1}",`, `          "command": │`, "            cd x", "            cat y",
		"        },", `        "other": "{\"a\":1}"`, "      }", "    ],", `    "toolResults": [`, "      {", `        "content": [`,
		"          1,", "          {", `            "content": "{\"b\":2}"`, "          }", "        ]", "      },", "      {",
		`        "content": │`, "          {", `          "a": 1}`, "      }", "    ]", "  },", `  "mcp": {`, `    "params": {`,
		`      "arguments": "{\"a\":1}"`, "    }", "  }", "}"}
	if got := treeText(t, doc, foldState{all: true}); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A string that is not exactly one JSON value stays a string, in arguments and in tool output.
func TestRenderTree_ToolJSONWithOtherBytesStaysAString(t *testing.T) {
	for _, s := range []string{`{bad`, `{} {}`, `{}]`, `{"a":1}}`, `{"a":1}]`, `{"a":1}x`} {
		for _, inf := range []map[string]any{
			{"toolCalls": []any{map[string]any{"arguments": s}}},
			{"toolResults": []any{map[string]any{"content": s}}},
		} {
			doc := toolDoc(t, map[string]any{"inference": inf})
			kept := toolDoc(t, s)
			if got := strings.Join(treeText(t, doc, foldState{all: true}), "\n"); !strings.Contains(got, kept) {
				t.Errorf("%s: got %q, want %s kept", doc, got, kept)
			}
		}
	}
}

// The exact capture shows the body as the client sent it: arguments and tool output stay strings.
func TestRenderTree_JSONStringInRawStaysAString(t *testing.T) {
	body := `{"raw":{"arguments":"{\"a\":1}","toolResults":[{"content":"[1]"}]},"inference":{"toolCalls":[{"arguments":"{\"a\":1}"}]}}`
	for _, path := range []string{"", "request", "response"} {
		sec, err := sectionTree(path, "━━ S", nil, []byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		var args, out, inf bool
		for _, l := range renderTree(sec, foldState{all: true}) {
			s := strings.TrimSpace(stripANSI(l.text))
			args = args || strings.HasPrefix(s, `"arguments": "{\"a\":1}"`)
			out = out || s == `"content": "[1]"`
			inf = inf || s == `"arguments": {`
		}
		if !args || !out || !inf {
			t.Errorf("path %q: raw arguments kept %v, raw output kept %v, inference object %v", path, args, out, inf)
		}
	}
}
