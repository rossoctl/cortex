package translate

import (
	"slices"

	"github.com/rossoctl/cortex/core/translate/anthropic"
	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
	"github.com/rossoctl/cortex/core/translate/responses"
)

// The registered protocol names.
const (
	Anthropic = "anthropic-messages"
	Chat      = "chat-completions"
	Responses = "responses"
)

// protocols is the compiled-in registry (rule H5). It has no
// registration API.
var protocols = map[string]pivot.Protocol{
	Anthropic: anthropic.Protocol{},
	Chat:      chat.Protocol{},
	Responses: responses.Protocol{},
}

// preference is the order in which Target picks a protocol (decision 12).
var preference = []string{Chat, Anthropic, Responses}

// Lookup gives the protocol with this name.
func Lookup(name string) (pivot.Protocol, bool) {
	p, ok := protocols[name]
	return p, ok
}

// Names gives the registered protocol names, sorted.
func Names() []string {
	names := make([]string, 0, len(protocols))
	for n := range protocols {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Decodes tells if the protocol can decode a request.
func Decodes(name string) bool {
	_, ok := protocols[name].(pivot.RequestDecoder)
	return ok
}

// Target gives the protocol to translate to, from the protocols that a
// server serves: the first in the preference order that has a request
// encoder (rule 4). false when there is none (rule 4a).
func Target(served []string) (string, bool) {
	for _, name := range preference {
		if _, ok := protocols[name].(pivot.RequestEncoder); ok && slices.Contains(served, name) {
			return name, true
		}
	}
	return "", false
}
