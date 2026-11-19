package model

import (
	"encoding/json"
	"io"
	"strings"
)

// ToolArgumentsJSON decodes s, a tool call's Arguments, when it is exactly one JSON object.
// A detail view shows that value, not the string. Numbers decode as json.Number.
func ToolArgumentsJSON(s string) (any, bool) {
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	return oneJSONValue(s)
}

// ToolOutputJSON decodes s, a tool result's Content, when it is one line and exactly one JSON
// object or array. Output with line breaks already reads well as a string.
func ToolOutputJSON(s string) (any, bool) {
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") || strings.Contains(s, "\n") {
		return nil, false
	}
	return oneJSONValue(s)
}

// oneJSONValue decodes s when it holds one JSON value with only white space after it.
func oneJSONValue(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}
