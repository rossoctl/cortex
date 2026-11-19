package responses

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// TestReasoningRoundTrip checks rule R14: the block comes back exactly.
func TestReasoningRoundTrip(t *testing.T) {
	for _, b := range []pivot.Block{
		{Kind: pivot.BlockThinking, Text: "Let me think <a> & \"b\".", Signature: "EqQBCkYIBxgCKkA+/="},
		{Kind: pivot.BlockRedactedThinking, Data: "cmVkYWN0ZWQ="},
		{Kind: pivot.BlockThinking, Text: "no signature"},
	} {
		enc := encodeReasoning(b)
		if !strings.HasPrefix(enc, "cortex1:") {
			t.Errorf("%q has no cortex1: prefix", enc)
		}
		if strings.Contains(enc, "=") {
			t.Errorf("%q has padding", enc)
		}
		got, ok := decodeReasoning(enc)
		if !ok || !reflect.DeepEqual(got, b) {
			t.Errorf("round trip of %+v gave %+v, %v", b, got, ok)
		}
	}
	for _, bad := range []string{"", "gAAAAABopenai", "cortex1:!!", "cortex1:" + "e30", "cortex1:eyJrIjoidGV4dCJ9"} {
		if _, ok := decodeReasoning(bad); ok {
			t.Errorf("decodeReasoning(%q) accepted it", bad)
		}
	}
}

// TestReasoningPayload checks the fixed field order of rule R14.
func TestReasoningPayload(t *testing.T) {
	got := encodeReasoning(pivot.Block{Kind: pivot.BlockThinking, Text: "x", Signature: "y"})
	want := "cortex1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"k":"thinking","t":"x","s":"y"}`))
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
