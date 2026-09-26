package plugins

import (
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// nonZeroCaps fills every field of PluginCapabilities with a non-zero value,
// driven by reflection rather than a hand-written literal. A field added to
// the struct with a kind this helper does not handle fails the test loudly,
// which is the point: it is impossible to add a capability and forget it here.
func nonZeroCaps(t *testing.T) pipeline.PluginCapabilities {
	t.Helper()
	var c pipeline.PluginCapabilities
	v := reflect.ValueOf(&c).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.String:
			f.SetString("value-" + name)
		case reflect.Slice:
			if f.Type().Elem().Kind() != reflect.String {
				t.Fatalf("PluginCapabilities.%s is a slice of %s — extend nonZeroCaps", name, f.Type().Elem().Kind())
			}
			f.Set(reflect.ValueOf([]string{"elem-" + name}))
		default:
			t.Fatalf("PluginCapabilities.%s has unhandled kind %s — extend nonZeroCaps", name, f.Kind())
		}
	}
	return c
}

// TestCloneCatalog_PreservesEveryCapabilityField is the regression test for the
// field-by-field copy that cloneCatalog used to do: it silently dropped any
// capability added later, so /v1/plugins under-reported. A struct copy picks
// new fields up automatically, and this test proves it for every field the
// struct has — now and after future additions.
func TestCloneCatalog_PreservesEveryCapabilityField(t *testing.T) {
	caps := nonZeroCaps(t)
	in := []CatalogEntry{{
		Name:         "probe",
		Capabilities: caps,
		Fields:       []pipeline.FieldSchema{{Name: "f"}},
	}}

	out := cloneCatalog(in)
	if len(out) != 1 {
		t.Fatalf("cloneCatalog returned %d entries, want 1", len(out))
	}
	if !reflect.DeepEqual(out[0].Capabilities, caps) {
		t.Errorf("capabilities not round-tripped:\n got %+v\nwant %+v", out[0].Capabilities, caps)
	}
	if out[0].Name != "probe" {
		t.Errorf("Name = %q, want %q", out[0].Name, "probe")
	}
}

// TestCloneCatalog_DeepCopiesEveryReferenceField walks PluginCapabilities by
// reflection and asserts that no field of a reference kind is aliased. The
// struct copy in cloneCatalog is correct for today's two slices, but a future
// map or slice capability would be silently shared with the registry — the same
// class of bug the field-by-field copy had, which is why this is driven by the
// struct rather than by a hand-written list.
func TestCloneCatalog_DeepCopiesEveryReferenceField(t *testing.T) {
	caps := nonZeroCaps(t)
	in := []CatalogEntry{{Name: "probe", Capabilities: caps}}
	out := cloneCatalog(in)

	src := reflect.ValueOf(&in[0].Capabilities).Elem()
	dst := reflect.ValueOf(&out[0].Capabilities).Elem()
	for i := 0; i < src.NumField(); i++ {
		name := src.Type().Field(i).Name
		switch src.Field(i).Kind() {
		case reflect.Slice:
			if src.Field(i).Len() == 0 {
				t.Fatalf("%s: nonZeroCaps left it empty, so aliasing cannot be detected", name)
			}
			if src.Field(i).UnsafePointer() == dst.Field(i).UnsafePointer() {
				t.Errorf("%s aliases the registry's slice", name)
			}
		case reflect.Map, reflect.Pointer:
			if src.Field(i).UnsafePointer() == dst.Field(i).UnsafePointer() {
				t.Errorf("%s is a %s shared with the registry — cloneCatalog needs to copy it",
					name, src.Field(i).Kind())
			}
		}
	}
}

// TestCloneCatalog_DeepCopiesSlices keeps the concrete mutation check: the clone
// must not alias the caller's slices, or a mutation through /v1/plugins would
// reach into the registry.
func TestCloneCatalog_DeepCopiesSlices(t *testing.T) {
	in := []CatalogEntry{{
		Name: "probe",
		Capabilities: pipeline.PluginCapabilities{
			Requires:    []string{"a"},
			RequiresAny: []string{"b"},
		},
	}}
	out := cloneCatalog(in)

	out[0].Capabilities.Requires[0] = "mutated"
	out[0].Capabilities.RequiresAny[0] = "mutated"

	if in[0].Capabilities.Requires[0] != "a" {
		t.Error("Requires aliases the input slice")
	}
	if in[0].Capabilities.RequiresAny[0] != "b" {
		t.Error("RequiresAny aliases the input slice")
	}
}
