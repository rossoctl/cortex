package responses

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// reasoningPrefix starts the encrypted_content that Cortex wrote (rule R14).
const reasoningPrefix = "cortex1:"

// reasoningPayload carries a pivot thinking block in encrypted_content.
// Do not change the field order: the bytes go back to the agent.
type reasoningPayload struct {
	K string `json:"k,omitempty"`
	T string `json:"t,omitempty"`
	S string `json:"s,omitempty"`
	D string `json:"d,omitempty"`
}

// encodeReasoning gives the encrypted_content for a thinking or
// redacted_thinking block (rule R14).
func encodeReasoning(b pivot.Block) string {
	// The value holds only strings, so marshal cannot fail.
	j, _ := marshal(reasoningPayload{K: string(b.Kind), T: b.Text, S: b.Signature, D: b.Data})
	return reasoningPrefix + base64.RawURLEncoding.EncodeToString(j)
}

// decodeReasoning restores the block from encrypted_content. false when
// Cortex did not write it, or when the payload is not valid.
func decodeReasoning(s string) (pivot.Block, bool) {
	rest, ok := strings.CutPrefix(s, reasoningPrefix)
	if !ok {
		return pivot.Block{}, false
	}
	j, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return pivot.Block{}, false
	}
	var p reasoningPayload
	if json.Unmarshal(j, &p) != nil {
		return pivot.Block{}, false
	}
	kind := pivot.BlockKind(p.K)
	if kind != pivot.BlockThinking && kind != pivot.BlockRedactedThinking {
		return pivot.Block{}, false
	}
	return pivot.Block{Kind: kind, Text: p.T, Signature: p.S, Data: p.D}, true
}
