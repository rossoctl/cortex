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
	// static-inject's host -> credential map; its values are secrets even
	// though the key name is not.
	"mappings",
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
			// Any value type, not only strings: an array or object under a
			// sensitive key (api_keys, credentials, mappings) holds secrets
			// too, and there is no shape worth preserving on a diagnostic
			// endpoint.
			m[k] = "[REDACTED]"
			continue
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
		// Match the plural too ("credentials", "api_keys"): a suffix check
		// on the singular never fires for it. "token" is doubly exceptional:
		// its plural matches every token-COUNT field (max_tokens,
		// input_tokens, …) in plugin config, which is served redacted on
		// /v1/pipeline and /config, and a count is not a secret; and
		// *_per_token is a pricing rate (config.Config's TierRates), so it
		// must survive for agentop exec's typed decode of /config.
		if s == "token" && strings.HasSuffix(lower, "per_token") {
			continue
		}
		if strings.HasSuffix(lower, s) || (s != "token" && strings.HasSuffix(lower, s+"s")) {
			return true
		}
	}
	return false
}
