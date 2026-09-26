package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

// Empty marshals as null, not as nothing.
//
// Empty bytes are not a JSON value, so returning them fails the ENCLOSING marshal — and a
// caller that inserts the schema unconditionally is not hypothetical: plugins/sparc's
// collector puts `"parameters": t.Parameters` into a map with no absence check, so an
// unpopulated schema there would have taken the whole request down.
func TestRawJSON_EmptyMarshalsAsNull(t *testing.T) {
	out, err := json.Marshal(RawJSON(""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); got != "null" {
		t.Errorf("marshal(empty) = %q, want null", got)
	}

	// The shape that matters: inside a map, unconditionally, as sparc does.
	out, err = json.Marshal(map[string]any{"parameters": RawJSON("")})
	if err != nil {
		t.Fatalf("marshal inside a map: %v", err)
	}
	if got, want := string(out), `{"parameters":null}`; got != want {
		t.Errorf("marshal = %q, want %q", got, want)
	}
}

// A populated value is emitted as-is rather than as a quoted string, which is the whole
// point of the type: it is a JSON value, not text that happens to contain JSON.
func TestRawJSON_MarshalsAsAValueNotAString(t *testing.T) {
	out, err := json.Marshal(map[string]any{"parameters": RawJSON(`{"type":"object"}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(out), `{"parameters":{"type":"object"}}`; got != want {
		t.Errorf("marshal = %q, want %q — a quoted string here means the type lost its Marshaler", got, want)
	}
}

// JSON null decodes to EMPTY, not to the four bytes "null".
//
// Keeping "null" would be four bytes long and pass every len(...) > 0 guard a consumer
// writes — plugins/opa checks exactly that before putting the schema into its policy input
// — handing rego a null where the map-typed field this replaced left the key absent.
func TestRawJSON_NullDecodesToEmpty(t *testing.T) {
	var doc struct {
		P RawJSON `json:"p"`
	}
	if err := json.Unmarshal([]byte(`{"p":null}`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.P != "" {
		t.Errorf("null decoded to %q (len %d), want empty", string(doc.P), len(doc.P))
	}
	if len(doc.P) > 0 {
		t.Error("a len > 0 guard would treat this absent schema as present")
	}
}

// Key order survives a decode/encode round trip, which decoding into a map is exactly what
// destroys: marshaling a map sorts its keys.
func TestRawJSON_RoundTripKeepsKeyOrder(t *testing.T) {
	const schema = `{"type":"object","properties":{"zebra":{"type":"string"},"alpha":{"type":"number"}},"required":["zebra"]}`

	var doc struct {
		P RawJSON `json:"p"`
	}
	if err := json.Unmarshal([]byte(`{"p":`+schema+`}`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(doc.P) != schema {
		t.Errorf("decode changed the value:\n got %s\nwant %s", doc.P, schema)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// "type" before "properties" before "required" — a map round trip would have sorted
	// them to properties, required, type.
	got := string(out)
	if strings.Index(got, `"type"`) > strings.Index(got, `"properties"`) {
		t.Errorf("keys were reordered: %s", got)
	}
	if strings.Index(got, `"zebra"`) > strings.Index(got, `"alpha"`) {
		t.Errorf("nested keys were reordered: %s", got)
	}
}

// What a re-marshal does NOT preserve, pinned so the doc comment stays honest: encoding/json
// compacts a Marshaler's output and HTML-escapes it.
func TestRawJSON_ReMarshalCompactsAndEscapes(t *testing.T) {
	out, err := json.Marshal(map[string]any{"p": RawJSON(`{"a": 1,  "b": "<x>&y"}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"a": 1`) {
		t.Errorf("expected the spacing to be compacted away, got %s", got)
	}
	if strings.Contains(got, "<x>") {
		t.Errorf("expected < and > to be escaped, got %s", got)
	}
	// The value itself is untouched — only the marshaled copy is compacted.
	v := RawJSON(`{"a": 1}`)
	if string(v) != `{"a": 1}` {
		t.Errorf("the held value changed: %q", string(v))
	}
}

// An InferenceTool with no schema must emit no "parameters" key at all, so a consumer can
// still tell absent from present.
func TestInferenceTool_OmitsAnEmptySchema(t *testing.T) {
	out, err := json.Marshal(InferenceTool{Name: "t", Description: "d"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "parameters") {
		t.Errorf("empty schema emitted a key: %s", out)
	}
}
