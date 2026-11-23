package inference

import (
	"testing"

	"github.com/rossoctl/cortex/core/translate"
)

// TestProtocolNames_MatchTranslateRegistry pins ProtocolFor's three return values against
// core/translate's registered protocol names (registry.go). Neither package imports the other,
// so nothing stops the two copies' values from drifting apart; this test is the thing that would
// catch it, since a renamed inference constant is already a compile error for its own callers
// (dialect.go), but a changed value on either side is not.
func TestProtocolNames_MatchTranslateRegistry(t *testing.T) {
	cases := []struct {
		name          string
		inferenceName string
		translateName string
	}{
		{"anthropic", ProtocolAnthropicMessages, translate.Anthropic},
		{"chat", ProtocolChatCompletions, translate.Chat},
		{"responses", ProtocolResponses, translate.Responses},
	}
	for _, c := range cases {
		if c.inferenceName != c.translateName {
			t.Errorf("%s: inference=%q, translate=%q, want equal", c.name, c.inferenceName, c.translateName)
		}
	}
}
