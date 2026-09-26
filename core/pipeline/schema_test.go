package pipeline

import (
	"reflect"
	"testing"
)

// Tests below use representative shapes covering every Type the
// helper recognizes plus the no-tag and pointer-to-struct cases.
// They lock in the contract that consumers (abctl template
// renderer, JSON-Schema generator) build on.

type primitives struct {
	Hostname string `json:"hostname" required:"true" description:"Target hostname."`
	Port     int    `json:"port"     description:"TCP port." default:"8080"`
	Verbose  bool   `json:"verbose"`
	Mode     string `json:"mode"     enum:"strict, lenient, off"`
}

func TestSchemaOf_Primitives(t *testing.T) {
	got := SchemaOf(primitives{})
	want := []FieldSchema{
		{Name: "hostname", Type: "string", Required: true, Description: "Target hostname."},
		{Name: "port", Type: "int", Description: "TCP port.", Default: "8080"},
		{Name: "verbose", Type: "bool"},
		{Name: "mode", Type: "string", Enum: []string{"strict", "lenient", "off"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SchemaOf primitives:\n got: %+v\nwant: %+v", got, want)
	}
}

type withList struct {
	Allowed []string `json:"allowed" description:"Allowlisted hosts."`
}

func TestSchemaOf_StringSlice(t *testing.T) {
	got := SchemaOf(withList{})
	if len(got) != 1 || got[0].Type != "[]string" {
		t.Errorf("expected one []string field, got %+v", got)
	}
}

type nestedInner struct {
	Path  string `json:"path" required:"true" description:"Routes file path."`
	Inner bool   `json:"inline"`
}

type nestedOuter struct {
	Name  string      `json:"name" required:"true"`
	Inner nestedInner `json:"inner" description:"Nested config block."`
}

func TestSchemaOf_NestedStruct(t *testing.T) {
	got := SchemaOf(nestedOuter{})
	if len(got) != 2 {
		t.Fatalf("expected 2 top-level fields, got %d (%+v)", len(got), got)
	}
	if got[1].Name != "inner" || got[1].Type != "object" {
		t.Errorf("nested field shape wrong: %+v", got[1])
	}
	if len(got[1].Fields) != 2 {
		t.Fatalf("nested fields not populated: %+v", got[1].Fields)
	}
	if got[1].Fields[0].Name != "path" || !got[1].Fields[0].Required {
		t.Errorf("nested[0] shape wrong: %+v", got[1].Fields[0])
	}
}

type withSkippable struct {
	JSONOmitted string `json:"-"`
	NoTag       string
	Counted     int `json:"counted"`
}

func TestSchemaOf_SkipsUntaggedAndDashed(t *testing.T) {
	// Skipping unexported fields is also part of the contract;
	// we don't unit-test that case here because adding an
	// unexported field with a json tag triggers `go vet`'s
	// structtag check and pollutes the test file. The
	// IsExported() guard in schemaOfType is straightforward.
	got := SchemaOf(withSkippable{})
	if len(got) != 1 || got[0].Name != "counted" {
		t.Errorf("expected only the `counted` field, got %+v", got)
	}
}

type withPointer struct {
	Optional *int `json:"optional" description:"Optional override."`
}

func TestSchemaOf_PointerField(t *testing.T) {
	got := SchemaOf(withPointer{})
	if len(got) != 1 || got[0].Type != "int" {
		t.Errorf("expected pointer-to-int to render as int, got %+v", got)
	}
}

func TestSchemaOf_PointerToStruct(t *testing.T) {
	got := SchemaOf(&primitives{})
	if len(got) != 4 {
		t.Errorf("expected SchemaOf to handle *struct, got %d fields", len(got))
	}
}

func TestSchemaOf_NotAStruct(t *testing.T) {
	if got := SchemaOf("not-a-struct"); got != nil {
		t.Errorf("expected nil for non-struct, got %+v", got)
	}
	if got := SchemaOf(42); got != nil {
		t.Errorf("expected nil for non-struct, got %+v", got)
	}
	if got := SchemaOf(nil); got != nil {
		t.Errorf("expected nil for nil, got %+v", got)
	}
}

type withSliceOfStruct struct {
	Routes []nestedInner `json:"routes" description:"List of routes."`
}

func TestSchemaOf_SliceOfStructIsUnknown(t *testing.T) {
	// We deliberately don't recurse into slice-of-struct — those
	// shapes don't fit the per-line YAML-scalar template model
	// abctl uses. Documented in kindOf comment.
	got := SchemaOf(withSliceOfStruct{})
	if len(got) != 1 || got[0].Type != "unknown" {
		t.Errorf("slice-of-struct should be \"unknown\", got %+v", got)
	}
}

// Empty-enum tag should produce nil, not [""]: the parser trims and
// drops empty parts.
func TestSchemaOf_EnumWhitespaceAndEmpty(t *testing.T) {
	type withMessyEnum struct {
		Mode string `json:"mode" enum:"  on , , off  "`
	}
	got := SchemaOf(withMessyEnum{})
	if len(got) != 1 || !reflect.DeepEqual(got[0].Enum, []string{"on", "off"}) {
		t.Errorf("expected trimmed [on, off], got %+v", got)
	}
}

// Self-referential pointer fields should not stack-overflow — the
// depth bound in schemaOfType caps recursion. A linked-list-style
// node (Next *node) is the canonical case.
func TestSchemaOf_SelfReferentialIsBounded(t *testing.T) {
	type node struct {
		Name string `json:"name"`
		Next *node  `json:"next"`
	}
	// Should return without panicking and emit a finite schema.
	got := SchemaOf(node{})
	if len(got) != 2 {
		t.Fatalf("expected 2 top-level fields, got %d", len(got))
	}
	// The recursion eventually returns nil at depth > maxSchemaDepth,
	// so somewhere in the nested chain Fields is nil. Walk down and
	// confirm we don't loop forever (the test itself would hang).
	depth := 0
	cur := got[1] // "next"
	for cur.Type == "object" && len(cur.Fields) >= 2 {
		depth++
		if depth > maxSchemaDepth+5 {
			t.Fatalf("recursion not bounded; reached depth %d", depth)
		}
		cur = cur.Fields[1]
	}
}
