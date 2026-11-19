package sessionexport

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// docPath is docs/session-export.md, which both documents the endpoint and states counts
// about this package as prose. The pins below hold those counts to the code.
func docPath() string {
	return filepath.Join("..", "..", "docs", "session-export.md")
}

// readDoc returns the page, or fails the test.
func readDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(docPath())
	if err != nil {
		t.Fatalf("read docs/session-export.md: %v", err)
	}
	return string(raw)
}

// docCount pulls the one number a pattern captures out of a document.
func docCount(t *testing.T, doc, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no count matches %q any more — if the sentence was reworded, "+
			"reword this pattern with it", pattern)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("count %q is not a number: %v", m[1], err)
	}
	return n
}

// TestDocFieldCountMatchesRecord pins the page's field count to Record.
//
// "What an export contains" tables every field, so a reader can decide whether an export is
// shareable before running one. That table is a hand-maintained copy of this struct, and
// the prose above it states the count. A field added to Record made the sentence quietly
// wrong, in the direction that matters: a reader would trust a list that no longer
// described the export (#1931 review).
//
// Neither existing pin covers this. TestDocExampleRecordMatchesWire pins the json example,
// and TestRecord_EveryFieldIsPopulated pins that FromEvent sets every field. Neither counts
// them.
//
// Two places state the count: the page and `agentop export --help`. Before #1931 the two
// disagreed — 36 against "~30" — which is how a hand-maintained count drifts.
func TestDocFieldCountMatchesRecord(t *testing.T) {
	want := reflect.TypeOf(Record{}).NumField()
	for _, c := range []struct {
		where, path, pattern string
	}{
		{"docs/session-export.md", docPath(), `A record holds (\d+) fields`},
		{"cmd/agentop/cmd_export.go", filepath.Join("..", "..", "cmd", "agentop", "cmd_export.go"),
			`reduced to (\d+) content-free scalars`},
	} {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.where, err)
		}
		if got := docCount(t, string(raw), c.pattern); got != want {
			t.Errorf("%s says %d fields, Record has %d — add the new field to the "+
				"\"What a record carries\" table, or to \"What a record never carries\" if it "+
				"must not be exported, and fix both counts", c.where, got, want)
		}
	}
}

// TestDocFieldNamesMatchRecord pins the page's table to Record field by field.
//
// The count alone is not enough: a rename (ttfbMs → ttfb) or a swap (one field added, one
// removed) keeps the count at 36 and leaves the table wrong (#1931 review). This compares
// the SET, so it names what drifted rather than reporting a number that still agrees.
func TestDocFieldNamesMatchRecord(t *testing.T) {
	want := map[string]bool{}
	rt := reflect.TypeOf(Record{})
	for i := range rt.NumField() {
		tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if tag != "" && tag != "-" {
			want[tag] = true
		}
	}

	doc := readDoc(t)
	const open, shut = "| Group | Fields |", "### Two fields are not short"
	i, j := strings.Index(doc, open), strings.Index(doc, shut)
	if i < 0 || j < i {
		t.Fatal("the field table is no longer between its heading and " +
			`"Two fields are not short" — retarget this pin`)
	}
	got := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-zA-Z]+)`").FindAllStringSubmatch(doc[i:j], -1) {
		got[m[1]] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("Record has %q, the \"What a record carries\" table does not — "+
				"add it to the table, or to \"What a record never carries\" if it must not "+
				"be exported", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("the table names %q, Record has no such json field — a renamed or "+
				"removed field leaves the table describing an export that does not exist", name)
		}
	}
}

// TestDocSecretCountMatchesAllSecrets pins the page's planted-secret count.
//
// The page cites the count as the evidence for the privacy contract. Understating it is the
// harmful direction: a 25th secret planted here without a doc edit leaves the page claiming
// a weaker guarantee than the one the test holds, and the number is the reason a reader
// believes the claim at all (#1931 review).
func TestDocSecretCountMatchesAllSecrets(t *testing.T) {
	want := len(allSecrets())
	if got := docCount(t, readDoc(t), `puts (\d+) distinct secrets`); got != want {
		t.Errorf("docs/session-export.md says the test plants %d secrets, allSecrets() has %d — "+
			"update the count in \"What a record never carries\"", got, want)
	}
}

// TestDocExampleRecordMatchesWire pins that the record example in docs/session-export.md
// strict-decodes into Record. The doc once showed promptTokens and completionTokens —
// fields Record deliberately has not, the very double count its own reading rules warn
// about — so every jq written against the example came back null (#1830 review). The
// strict decode is the pin: a field the projection does not emit fails here before it
// ships again, the way sessionapi's TestDocExamplePromptContextMatchesWire holds its doc.
//
// It finds the example under "What a record is" rather than taking the FIRST json block on
// the page. The page has three, and the walkthrough's short sample comes first: anchoring
// on position made this test fail for the reason that the wrong block has no cache figures,
// which says nothing about whether the real example is right (#1931).
func TestDocExampleRecordMatchesWire(t *testing.T) {
	raw, err := os.ReadFile(docPath())
	if err != nil {
		t.Fatalf("read docs/session-export.md: %v", err)
	}
	doc := string(raw)
	const section = "## What a record is"
	at := strings.Index(doc, section)
	if at < 0 {
		t.Fatalf("docs/session-export.md has no %q section — retarget this pin", section)
	}
	start := strings.Index(doc[at:], "```json\n")
	if start < 0 {
		t.Fatalf("docs/session-export.md's %q section carries no json example", section)
	}
	start += at
	scope := doc[start+len("```json\n"):]
	end := strings.Index(scope, "```")
	if end < 0 {
		t.Fatal("docs/session-export.md's json example is never closed")
	}
	example := scope[:end]

	dec := json.NewDecoder(bytes.NewReader([]byte(example)))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("the example does not strict-decode into Record: %v\nexample:\n%s", err, example)
	}
	if r.Schema != SchemaVersion {
		t.Errorf("example schema = %d, want %d", r.Schema, SchemaVersion)
	}
	// What every record carries, and the token figures an example exists to show: a doc
	// example with zeros teaches the zero-means-absent rule backwards.
	if r.SessionID == "" || r.Seq == 0 || r.At.IsZero() || r.Host == "" {
		t.Errorf("example is missing the fields every record carries: %+v", r)
	}
	if r.InputTokens == 0 || r.CacheReadTokens == 0 || r.OutputTokens == 0 {
		t.Errorf("example carries no real token figures: %+v", r)
	}
}
