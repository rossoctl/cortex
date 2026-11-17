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

// TestDocExampleRecordMatchesWire pins that the record example in docs/session-export-reference.md
// strict-decodes into Record. The doc once showed promptTokens and completionTokens —
// fields Record deliberately has not, the very double count its own reading rules warn
// about — so every jq written against the example came back null (#1830 review). The
// strict decode is the pin: a field the projection does not emit fails here before it
// ships again, the way sessionapi's TestDocExamplePromptContextMatchesWire holds its doc.
func TestDocExampleRecordMatchesWire(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-export-reference.md"))
	if err != nil {
		t.Fatalf("read docs/session-export-reference.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "```json\n")
	if start < 0 {
		t.Fatal("docs/session-export-reference.md carries no json example")
	}
	scope := doc[start+len("```json\n"):]
	end := strings.Index(scope, "```")
	if end < 0 {
		t.Fatal("docs/session-export-reference.md's json example is never closed")
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

// readGuide returns docs/session-export-guide.md, which states two counts about this
// package as prose.
func readGuide(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-export-guide.md"))
	if err != nil {
		t.Fatalf("read docs/session-export-guide.md: %v", err)
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

// TestDocGuideFieldCountMatchesRecord pins the guide's field count to Record.
//
// The guide's "What an export contains" section tables every field, so a reader can decide
// whether an export is shareable before running one. That table is a hand-maintained copy
// of this struct, and the prose above it states the count. A field added to Record made the
// sentence quietly wrong, in the direction that matters: a reader would trust a list that
// no longer described the export (#1931 review).
//
// Neither existing pin covers this. TestDocExampleRecordMatchesWire pins the REFERENCE
// page's json example, and TestRecord_EveryFieldIsPopulated pins that FromEvent sets every
// field. Neither counts them.
// Three places state the field count: the guide's table, the reference's one-line
// summary, and `agentop export --help`. Before #1931 the guide said 36 while the other
// two said "about 30", which is how a hand-maintained count drifts.
func TestDocGuideFieldCountMatchesRecord(t *testing.T) {
	want := reflect.TypeOf(Record{}).NumField()
	for _, c := range []struct {
		where, path, pattern string
	}{
		{"docs/session-export-guide.md", filepath.Join("..", "..", "docs", "session-export-guide.md"),
			`A record holds (\d+) fields`},
		{"docs/session-export-reference.md", filepath.Join("..", "..", "docs", "session-export-reference.md"),
			`an allowlist of (\d+)\s+scalars`},
		{"cmd/agentop/cmd_export.go", filepath.Join("..", "..", "cmd", "agentop", "cmd_export.go"),
			`reduced to (\d+) content-free scalars`},
	} {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.where, err)
		}
		if got := docCount(t, string(raw), c.pattern); got != want {
			t.Errorf("%s says %d fields, Record has %d — add the new field to the guide's "+
				"\"What a record carries\" table, or to \"What a record never carries\" if it "+
				"must not be exported, and fix all three counts", c.where, got, want)
		}
	}
}

// TestDocGuideSecretCountMatchesAllSecrets pins the guide's planted-secret count.
//
// The guide cites the count as the evidence for the privacy contract. Understating it is
// the harmful direction: a 25th secret planted here without a doc edit leaves the guide
// claiming a weaker guarantee than the one the test holds, and the number is the reason a
// reader believes the claim at all (#1931 review).
func TestDocGuideSecretCountMatchesAllSecrets(t *testing.T) {
	want := len(allSecrets())
	if got := docCount(t, readGuide(t), `puts (\d+) distinct secrets`); got != want {
		t.Errorf("docs/session-export-guide.md says the test plants %d secrets, allSecrets() has %d — "+
			"update the count in \"What a record never carries\"", got, want)
	}
}
