// Package chat is the chat-completions protocol of the translation hub.
// It encodes pivot requests as OpenAI /v1/chat/completions bodies, and
// decodes chat-completions replies to pivot events (rules 11-24 of #1911).
// It also decodes chat-completions requests to the pivot form (rules K3
// and K4 of #1966), and encodes pivot reply events as chat-completions
// replies (rules K5-K8 of #1966).
package chat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// Protocol is the chat-completions protocol.
type Protocol struct{}

// Name is the protocol name.
const Name = "chat-completions"

// extraPrefix starts each pivot.Extra key that this protocol writes.
const extraPrefix = "chat-completions."

var (
	_ pivot.Protocol            = Protocol{}
	_ pivot.RequestEncoder      = Protocol{}
	_ pivot.ReplyDecoding       = Protocol{}
	_ pivot.RequestDecoder      = Protocol{}
	_ pivot.SourceHeaderRemover = Protocol{}
)

// Name gives the protocol name.
func (Protocol) Name() string { return Name }

// RemoveSourceHeaders removes the headers that address OpenAI's service
// (rule K2 of #1966): OpenAI-Beta, OpenAI-Organization and OpenAI-Project.
func (Protocol) RemoveSourceHeaders(h http.Header) {
	for k := range h {
		switch strings.ToLower(k) {
		case "openai-beta", "openai-organization", "openai-project":
			delete(h, k)
		}
	}
}

// marshal writes v with no HTML escapes, so that "<" in a prompt goes to
// the server as "<". The output is the same for the same value (rule 9a).
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
