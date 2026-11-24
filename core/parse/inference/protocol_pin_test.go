package inference_test

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/translate"
)

// TestProtocolFor_MatchesTranslateRegistry pins what ProtocolFor actually returns, for a path of
// each dialect, against core/translate's registered protocol names (registry.go), and also pins
// the two sets of names equal in both directions: a protocol translate knows that ProtocolFor
// could never route to would fail here too. Neither package imports the other, so nothing else
// catches either kind of drift. package inference_test (not inference) only, because it uses
// nothing unexported, and because an inference-side follow-up that has translate import
// inference for one source of truth would otherwise turn an internal copy of this file into an
// import cycle.
func TestProtocolFor_MatchesTranslateRegistry(t *testing.T) {
	cases := []struct {
		name, path, translateName string
	}{
		{"anthropic", "/v1/messages", translate.Anthropic},
		{"chat", "/v1/chat/completions", translate.Chat},
		{"responses", "/v1/responses", translate.Responses},
	}
	for _, c := range cases {
		if got := inference.ProtocolFor(c.path); got != c.translateName {
			t.Errorf("%s: ProtocolFor(%q) = %q, translate registry name = %q", c.name, c.path, got, c.translateName)
		}
	}

	got := []string{inference.ProtocolAnthropicMessages, inference.ProtocolChatCompletions, inference.ProtocolResponses}
	slices.Sort(got)
	if want := translate.Names(); !slices.Equal(got, want) {
		t.Errorf("inference's protocol names = %q, translate registry = %q, want equal", got, want)
	}
}
