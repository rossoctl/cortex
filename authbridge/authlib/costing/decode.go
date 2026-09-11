package costing

import "encoding/json"

// The published plugin events are plugin-defined structs this package deliberately does not
// import. These read the few fields needed through the same JSON tags the wire uses, so a
// rename in the producer is caught by costing's own tests rather than silently zeroing a
// saving.

func asMap(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}
