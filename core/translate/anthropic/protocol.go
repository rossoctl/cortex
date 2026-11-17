// Package anthropic is the anthropic-messages translation protocol: the
// Anthropic Messages API. It decodes a request to the pivot form and
// encodes pivot reply events as an Anthropic reply (rule H6).
package anthropic

import "github.com/rossoctl/cortex/core/translate/pivot"

// Name is the protocol name.
const Name = "anthropic-messages"

// extraPrefix starts each pivot.Extra key that this protocol writes.
const extraPrefix = "anthropic."

// Protocol is the anthropic-messages protocol.
type Protocol struct{}

var (
	_ pivot.Protocol       = Protocol{}
	_ pivot.RequestDecoder = Protocol{}
	_ pivot.ReplyEncoding  = Protocol{}
)

// Name gives "anthropic-messages".
func (Protocol) Name() string { return Name }
