// Package redact provides best-effort stripping of sensitive values from
// JSON config payloads served by unauthenticated diagnostic endpoints.
// The canonical defense is to keep inline secrets out of config entirely
// (use *_file paths instead); this layer is defense-in-depth.
package redact

import (
	"encoding/json"
	"strings"
)

var sensitiveKeys = []string{
	"secret", "password", "token", "bearer", "key", "credential",
}

// JSON redacts values whose keys match sensitive patterns in a
// json.RawMessage. Non-object inputs are returned unchanged.
func JSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}

	redactMap(m)

	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if isSensitiveKey(k) {
			if _, ok := v.(string); ok {
				m[k] = "[REDACTED]"
				continue
			}
		}
		switch val := v.(type) {
		case map[string]any:
			redactMap(val)
		case []any:
			redactSlice(val)
		}
	}
}

func redactSlice(s []any) {
	for _, v := range s {
		switch val := v.(type) {
		case map[string]any:
			redactMap(val)
		case []any:
			redactSlice(val)
		}
	}
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}
