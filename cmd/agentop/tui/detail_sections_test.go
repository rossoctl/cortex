package tui

import (
	"strings"
	"testing"
)

func sectionLines(t *testing.T, sec *node, fs foldState) []treeLine {
	t.Helper()
	return renderTree(sectionsRoot(sec), fs)
}

func TestSectionTree_TitleHeadsAndPaths(t *testing.T) {
	sec, err := sectionTree("response", "━━ RESPONSE · 14:00:00.00", []string{"served:      m"},
		[]byte(`{"host":"h","inference":{"model":"m"}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	ls := sectionLines(t, sec, foldState{})
	if got := stripANSI(ls[0].text); !strings.Contains(got, "━━ RESPONSE") || ls[0].path != "response" || !ls[0].head {
		t.Errorf("line 0 = %q path %q head %v, want the section title as a fold head", got, ls[0].path, ls[0].head)
	}
	if got := stripANSI(ls[1].text); !strings.Contains(got, "served:") {
		t.Errorf("line 1 = %q, want the section's header line", got)
	}
	for _, l := range ls {
		if l.path != "response" && !strings.HasPrefix(l.path, "response.") {
			t.Errorf("line %q has path %q, want it under response", stripANSI(l.text), l.path)
		}
	}
}

func TestSectionTree_FoldsAsOne(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil, []byte(`{"host":"h"}`), "")
	ls := sectionLines(t, sec, foldState{open: map[string]foldChoice{"request": {open: false}}})
	if len(ls) != 1 {
		t.Errorf("%d lines, want only the closed title", len(ls))
	}
}

func TestSectionTree_MissingHalfShowsNote(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil, nil, "request not loaded: press o in events")
	ls := sectionLines(t, sec, foldState{})
	if len(ls) != 2 || !strings.Contains(stripANSI(ls[1].text), "request not loaded") {
		t.Errorf("lines = %v, want the title and the note", lineTexts(ls))
	}
}

// The section title names the plugins that acted, so the full list starts closed.
func TestSectionTree_InvocationsStartFolded(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil,
		[]byte(`{"invocations":{"outbound":[{"plugin":"tool-prune","action":"skip"}]}}`), "")
	text := stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{})), "\n"))
	if !strings.Contains(text, `▸ "invocations"`) || strings.Contains(text, "tool-prune") {
		t.Errorf("want invocations closed:\n%s", text)
	}
	// A search with a match inside opens it, as for any fold.
	text = stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{query: "tool-prune"})), "\n"))
	if !strings.Contains(text, "tool-prune") {
		t.Errorf("a search for a skipped plugin did not open invocations:\n%s", text)
	}
}

// The raw capture holds a second copy of the conversation. A query never opens it, under a
// section path as at the root.
func TestSectionTree_QueryDoesNotOpenRaw(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil,
		[]byte(`{"host":"needle","raw":{"body":"needle"}}`), "")
	text := stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{query: "needle"})), "\n"))
	if strings.Contains(text, `"body"`) {
		t.Errorf("the query opened raw:\n%s", text)
	}
	// A match that only raw holds does not open a section the reader closed. hasMatch does
	// not go into raw, so a search does not scan up to 10 MiB on each keystroke.
	sec, _ = sectionTree("request", "━━ REQUEST", nil, []byte(`{"host":"h","raw":{"body":"needle"}}`), "")
	ls := sectionLines(t, sec, foldState{query: "needle", open: map[string]foldChoice{"request": {open: false}}})
	if len(ls) != 1 {
		t.Errorf("lines = %v, want only the closed title: the match is only in raw", lineTexts(ls))
	}
}

func TestSectionOf(t *testing.T) {
	for path, want := range map[string]string{
		"response": "response", "response.host": "response", "response.inference.messages[0]": "response",
		"request": "request", "request.host": "request", "": "request",
	} {
		if got := sectionOf(path); got != want {
			t.Errorf("sectionOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDropShared(t *testing.T) {
	req := []byte(`{"host":"a","httpMethod":"POST","at":"1","sessionId":"s"}`)
	resp := []byte(`{"host":"b","httpMethod":"POST","at":"2","sessionId":"s","statusCode":200}`)
	if got, want := string(dropShared(resp, req)), `{"at":"2","host":"b","statusCode":200}`; got != want {
		t.Errorf("dropShared = %s, want %s: equal fields go, a different host and at stay", got, want)
	}
}
