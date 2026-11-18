// Package anthropic is the anthropic-messages translation protocol: the
// Anthropic Messages API. It decodes a request to the pivot form,
// encodes a pivot request as an Anthropic request, encodes pivot reply
// events as an Anthropic reply, and decodes an Anthropic reply to pivot
// events (rule H6).
package anthropic

import (
	"net/http"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// Name is the protocol name.
const Name = "anthropic-messages"

// extraPrefix starts each pivot.Extra key that this protocol writes.
const extraPrefix = "anthropic."

// Protocol is the anthropic-messages protocol.
type Protocol struct{}

var (
	_ pivot.Protocol            = Protocol{}
	_ pivot.RequestDecoder      = Protocol{}
	_ pivot.RequestEncoder      = Protocol{}
	_ pivot.ReplyEncoding       = Protocol{}
	_ pivot.ReplyDecoding       = Protocol{}
	_ pivot.SourceHeaderRemover = Protocol{}
)

// Name gives "anthropic-messages".
func (Protocol) Name() string { return Name }

// RemoveSourceHeaders removes the Anthropic headers, which address
// Anthropic's service (rule R3).
func (Protocol) RemoveSourceHeaders(h http.Header) {
	h.Del("Anthropic-Version")
	h.Del("Anthropic-Beta")
}
