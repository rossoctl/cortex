// Package chat is the chat-completions protocol of the translation hub.
// It encodes pivot requests as OpenAI /v1/chat/completions bodies, and
// decodes chat-completions replies to pivot events (rules 11-24 of #1911).
package chat

import (
	"bytes"
	"encoding/json"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// Protocol is the chat-completions protocol.
type Protocol struct{}

var (
	_ pivot.Protocol       = Protocol{}
	_ pivot.RequestEncoder = Protocol{}
	_ pivot.ReplyDecoding  = Protocol{}
)

// Name gives the protocol name.
func (Protocol) Name() string { return "chat-completions" }

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
