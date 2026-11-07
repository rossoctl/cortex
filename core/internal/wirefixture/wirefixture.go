// Package wirefixture checks a handler's JSON against a shared fixture file.
//
// The fixture holds the keys that a client (agentop) decodes. The proxy test calls
// Covers on the real handler output. The client test decodes the same file with
// DisallowUnknownFields. Thus a renamed JSON tag on either side fails a test.
package wirefixture

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Covers fails t unless the outputs have every key of the fixture at path, with
// the same JSON type. The keys of all outputs, and of all elements of an array,
// are merged, because one value seldom sets every optional field.
func Covers(t testing.TB, path string, outputs ...[]byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	var have []any
	for _, got := range outputs {
		var v any
		if err := json.Unmarshal(got, &v); err != nil {
			t.Fatalf("handler output is not JSON: %v\n%s", err, got)
		}
		have = append(have, v)
	}
	for _, msg := range covers("$", want, merge(have)) {
		t.Errorf("%s: %s", path, msg)
	}
}

func covers(at string, want, have any) []string {
	if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", have) {
		return []string{fmt.Sprintf("%s is %T in the fixture, %T in the output", at, want, have)}
	}
	var out []string
	switch w := want.(type) {
	case map[string]any:
		h := have.(map[string]any)
		for k, wv := range w {
			if hv, ok := h[k]; ok {
				out = append(out, covers(at+"."+k, wv, hv)...)
			} else {
				out = append(out, at+"."+k+" is missing from the output")
			}
		}
	case []any:
		h := have.([]any)
		if len(w) > 0 && len(h) == 0 {
			return []string{at + " is empty in the output, so its elements cannot be checked"}
		}
		for _, wv := range w {
			out = append(out, covers(at+"[]", wv, merge(h))...)
		}
	}
	return out
}

// merge folds values into one: the union of the keys of objects, the merged
// elements of arrays, and the first non-null scalar.
func merge(vs []any) any {
	var scalar any
	var objs map[string][]any
	var elems []any
	isArr := false
	for _, v := range vs {
		switch x := v.(type) {
		case map[string]any:
			if objs == nil {
				objs = map[string][]any{}
			}
			for k, kv := range x {
				objs[k] = append(objs[k], kv)
			}
		case []any:
			isArr = true
			elems = append(elems, x...)
		default:
			if scalar == nil {
				scalar = x
			}
		}
	}
	switch {
	case objs != nil:
		m := map[string]any{}
		for k, kvs := range objs {
			m[k] = merge(kvs)
		}
		return m
	case isArr && len(elems) == 0:
		return []any{}
	case isArr:
		return []any{merge(elems)}
	}
	return scalar
}
