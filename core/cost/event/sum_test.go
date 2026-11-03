package event

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestAddSat(t *testing.T) {
	for _, tc := range []struct {
		name      string
		a, b      int64
		want      int64
		saturated bool
	}{
		{"in range", 2, 3, 5, false},
		{"exactly the ceiling", math.MaxInt64 - 1, 1, math.MaxInt64, false},
		{"past the ceiling", math.MaxInt64, 1, math.MaxInt64, true},
		{"exactly the floor", math.MinInt64 + 1, -1, math.MinInt64, false},
		{"past the floor", math.MinInt64, -1, math.MinInt64, true},
		{"mixed signs never clamp", math.MaxInt64, math.MinInt64, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, saturated := AddSat(tc.a, tc.b)
			if got != tc.want || saturated != tc.saturated {
				t.Errorf("AddSat(%d, %d) = (%d, %v), want (%d, %v)",
					tc.a, tc.b, got, saturated, tc.want, tc.saturated)
			}
		})
	}
}

// The flag is sticky: a later in-range Add leaves the figure a bound.
func TestCostSum_AddKeepsTheClampDisclosed(t *testing.T) {
	var s CostSum
	s.Add(math.MaxInt64)
	if s.Saturated {
		t.Fatal("Saturated set by an in-range Add")
	}
	s.Add(1)
	if s.Micros != math.MaxInt64 || !s.Saturated {
		t.Fatalf("after overflowing: %+v, want MaxInt64 and Saturated", s)
	}
	s.Add(-1)
	if !s.Saturated {
		t.Error("an in-range Add after a clamp cleared Saturated")
	}
}

func TestCostSum_SubClampsBothWays(t *testing.T) {
	var down CostSum
	down.Add(math.MinInt64)
	down.Sub(1)
	if down.Micros != math.MinInt64 || !down.Saturated {
		t.Errorf("MinInt64 - 1 = %+v, want MinInt64 and Saturated", down)
	}

	// Negating math.MinInt64 is math.MinInt64 again, so a naive Add(-micros) would add it.
	var up CostSum
	up.Add(1_000)
	up.Sub(math.MinInt64)
	if up.Micros != math.MaxInt64 || !up.Saturated {
		t.Errorf("1000 - MinInt64 = %+v, want MaxInt64 and Saturated", up)
	}

	var exact CostSum
	exact.Add(1_000)
	exact.Sub(250)
	if exact.Micros != 750 || exact.Saturated {
		t.Errorf("1000 - 250 = %+v, want 750 and not Saturated", exact)
	}
}

func TestAgentLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *pipeline.EventClient
		want string
	}{
		{"nil is unknown", nil, pipeline.UnknownClientLabel},
		{"a version folds away", &pipeline.EventClient{Name: "claude-code", Version: "2.1.285"}, "claude-code"},
		{"a retired spelling folds to its replacement", &pipeline.EventClient{Name: "bob-shell", Version: "2.2.1"}, "bob"},
		{"a control byte is replaced", &pipeline.EventClient{Raw: "tool\x1b[2J"}, "tool�[2J"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentLabel(tc.c); got != tc.want {
				t.Errorf("AgentLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// MaxLabelLen control bytes become three times as many bytes once sanitized, so the cap
// holds only if it is applied second.
func TestAgentLabel_SanitizesBeforeCapping(t *testing.T) {
	got := AgentLabel(&pipeline.EventClient{Raw: strings.Repeat("\x1b", MaxLabelLen)})
	if len(got) > MaxLabelLen {
		t.Errorf("AgentLabel returned %d bytes, cap is %d", len(got), MaxLabelLen)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("AgentLabel kept an ESC byte: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("AgentLabel returned invalid UTF-8: %q", got)
	}
}
