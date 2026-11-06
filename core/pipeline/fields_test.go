package pipeline

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateFields = flag.Bool("update", false, "rewrite testdata/parsed-request.fields from the extension types")

// parsedRequestFields is the JSON field list of the three parser extensions.
// agentop and the script SDK read this JSON from session events.
const parsedRequestFields = "testdata/parsed-request.fields"

var jsonMarshaler = reflect.TypeFor[json.Marshaler]()

// fieldLines gives one "path kind" line for each JSON field of the three parser extensions.
// A path uses "[]" for an array item and "{}" for a map value.
// A kind is string, number, bool, object, array, map or any.
// "any" is a value with no fixed shape: an interface, or a type with its own MarshalJSON.
func fieldLines() []string {
	var lines []string
	var walk func(path string, t reflect.Type)
	walk = func(path string, t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) {
			lines = append(lines, path+" any")
			return
		}
		switch k := t.Kind(); {
		case k == reflect.String:
			lines = append(lines, path+" string")
		case k == reflect.Bool:
			lines = append(lines, path+" bool")
		case k >= reflect.Int && k <= reflect.Float64:
			lines = append(lines, path+" number")
		case k == reflect.Slice || k == reflect.Array:
			lines = append(lines, path+" array")
			walk(path+"[]", t.Elem())
		case k == reflect.Map:
			lines = append(lines, path+" map")
			walk(path+"{}", t.Elem())
		case k == reflect.Struct:
			lines = append(lines, path+" object")
			walkStruct(path, t, walk)
		default:
			lines = append(lines, path+" any")
		}
	}
	walkStruct("inference", reflect.TypeFor[InferenceExtension](), walk)
	walkStruct("mcp", reflect.TypeFor[MCPExtension](), walk)
	walkStruct("a2a", reflect.TypeFor[A2AExtension](), walk)
	sort.Strings(lines)
	return lines
}

// walkStruct calls walk for each JSON field of struct t, with the name encoding/json gives it.
func walkStruct(path string, t reflect.Type, walk func(string, reflect.Type)) {
	for _, f := range reflect.VisibleFields(t) {
		tag := f.Tag.Get("json")
		if !f.IsExported() || f.Anonymous || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		walk(path+"."+name, f.Type)
	}
}

// TestParsedRequestFields compares the JSON shape of the parser extensions to testdata/parsed-request.fields.
// A changed JSON tag, an added field or a removed field fails it.
// Run with -update to rewrite the file, and update docs/session-api.md in the same change.
func TestParsedRequestFields(t *testing.T) {
	got := strings.Join(fieldLines(), "\n") + "\n"
	if *updateFields {
		if err := os.WriteFile(parsedRequestFields, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parsedRequestFields)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("the extension types do not agree with %s (run with -update to rewrite it)\ngot:\n%s", parsedRequestFields, got)
	}
}

// TestParsedRequestFieldsDocumented fails when docs/session-api.md does not name a top-level field of an extension.
// The doc must contain the field as "<extension>.<field>", for example inference.model.
func TestParsedRequestFieldsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/session-api.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var missing []string
	for _, line := range fieldLines() {
		path, _, _ := strings.Cut(line, " ")
		ext, rest, _ := strings.Cut(path, ".")
		field := ext + "." + strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune(".[{", r) })[0]
		if seen[field] {
			continue
		}
		seen[field] = true
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\b`).Match(doc) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/session-api.md does not name %d fields. Add each one to the event schema:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}
