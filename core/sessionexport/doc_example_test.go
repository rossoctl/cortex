package sessionexport

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocExampleRecordMatchesWire pins that the record example in docs/session-export.md
// strict-decodes into Record. The doc once showed promptTokens and completionTokens —
// fields Record deliberately has not, the very double count its own reading rules warn
// about — so every jq written against the example came back null (#1830 review). The
// strict decode is the pin: a field the projection does not emit fails here before it
// ships again, the way sessionapi's TestDocExamplePromptContextMatchesWire holds its doc.
func TestDocExampleRecordMatchesWire(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-export.md"))
	if err != nil {
		t.Fatalf("read docs/session-export.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "```json\n")
	if start < 0 {
		t.Fatal("docs/session-export.md carries no json example")
	}
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
