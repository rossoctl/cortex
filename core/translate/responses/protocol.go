// Package responses is the responses protocol of the translation hub: the
// OpenAI Responses API that Codex sends. It decodes a request to the pivot
// form (rules R5-R10 and R14 of #1964).
package responses

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// Name is the protocol name.
const Name = "responses"

// extraPrefix starts each pivot.Extra key that this protocol writes.
const extraPrefix = "responses."

// Protocol is the responses protocol.
type Protocol struct{}

var (
	_ pivot.Protocol            = Protocol{}
	_ pivot.RequestDecoder      = Protocol{}
	_ pivot.SourceHeaderRemover = Protocol{}
)

// Name gives "responses".
func (Protocol) Name() string { return Name }

// sourceHeaders are the headers that address OpenAI's service (rule R3),
// in lower case.
var sourceHeaders = map[string]bool{
	"openai-beta":         true,
	"openai-organization": true,
	"openai-project":      true,
	"chatgpt-account-id":  true,
	"originator":          true,
}

// RemoveSourceHeaders removes the OpenAI and Codex headers (rule R3).
func (Protocol) RemoveSourceHeaders(h http.Header) {
	for k := range h {
		l := strings.ToLower(k)
		if sourceHeaders[l] || strings.HasPrefix(l, "x-codex-") || strings.HasPrefix(l, "x-openai-") {
			delete(h, k)
		}
	}
}

// marshal writes v with no HTML escapes. The output is the same for the
// same value (rule 9a of #1911).
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
