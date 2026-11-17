package anthropic

import (
	"bytes"
	"encoding/json"
)

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorBody struct {
	Type  string      `json:"type"`
	Error errorDetail `json:"error"`
}

// EncodeError gives an Anthropic error body (rule 24). The status goes on
// the HTTP reply, not in the body.
func (Protocol) EncodeError(_ int, errType, message string) []byte {
	return marshal(errorBody{Type: "error", Error: errorDetail{Type: errType, Message: message}})
}

// marshal encodes v with no HTML escapes, so text goes to the agent as
// the server wrote it. v holds only fixed types and checked raw JSON, so
// an error is a bug.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("anthropic: marshal: " + err.Error())
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
