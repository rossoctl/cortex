package parsercommon

import "testing"

// Kind values are serialized into session events (via ext.PresentKinds)
// and consumed by the log renderer's -1 sentinel logic, so their numeric
// layout is a wire contract, not just an internal iota.
func TestKindBitLayout(t *testing.T) {
	cases := []struct {
		name string
		got  Kind
		want Kind
	}{
		{"KindInput", KindInput, 1},
		{"KindCacheRead", KindCacheRead, 2},
		{"KindCacheWrite", KindCacheWrite, 4},
		{"KindOutput", KindOutput, 8},
		{"KindReasoning", KindReasoning, 16},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}
