package parsercommon

import (
	"testing"

	"github.com/rossoctl/cortex/core/usage"
)

// Kind IS the authority for the PresentKinds bit layout, and usage exports a copy of it
// because nothing outside core/plugins can import this package. This is the one place both
// sets are visible, so it is the only place the copy can be checked.
//
// WHY THE COPY EXISTS AT ALL. usage.Counts.PresentKinds carries these bits on the wire and in
// the cost ledger, and its consumers — abctl's `cost` command, abctl's spend strip — live in
// another module entirely. Before usage exported them, each reader spelled the bits itself and
// this package's own tests spelled them a third time as a bare `1 | 8`. Three uncoordinated
// transcriptions of a wire format is a renumbering away from silently misreading every stored
// snapshot; one exported set plus this test is the version that cannot drift.
//
// EVERY BIT, and the WIDTH. A test covering four of five would leave the fifth free to move,
// and PresentKinds is a uint8 where Kind is a uint8 — a widening on either side changes what
// the field can hold.
func TestKindBits_MatchTheParserThatProducesThem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mine   Kind
		theirs uint8
	}{
		{"Input", KindInput, usage.KindInput},
		{"CacheRead", KindCacheRead, usage.KindCacheRead},
		{"CacheWrite", KindCacheWrite, usage.KindCacheWrite},
		{"Output", KindOutput, usage.KindOutput},
		{"Reasoning", KindReasoning, usage.KindReasoning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if uint8(tc.mine) != tc.theirs {
				t.Errorf("parsercommon.Kind%s = %d but usage.Kind%s = %d: these are one wire "+
					"format, and a consumer reading PresentKinds with the wrong bit reports the "+
					"wrong token kind for every stored snapshot", tc.name, tc.mine, tc.name, tc.theirs)
			}
		})
	}
	// A bit added on one side and not the other is the drift this cannot see field-by-field,
	// so the SET is compared too: every bit either side defines has to appear above.
	var mine, theirs uint8
	for _, k := range []Kind{KindInput, KindCacheRead, KindCacheWrite, KindOutput, KindReasoning} {
		mine |= uint8(k)
	}
	for _, k := range []uint8{usage.KindInput, usage.KindCacheRead, usage.KindCacheWrite,
		usage.KindOutput, usage.KindReasoning} {
		theirs |= k
	}
	if mine != theirs || mine != 0b11111 {
		t.Errorf("the kind sets are %05b (parser) and %05b (usage), want both %05b — a kind "+
			"added to one side has to be added to the other and listed here", mine, theirs, 0b11111)
	}
}
