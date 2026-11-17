package contextguru

import (
	"testing"

	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/accounting"
)

// accounting.Components is the allow-list of the savings counters' `component` label. It is a
// copy, because core/accounting may not import context-guru; this test fails when a context-guru
// bump registers a component the copy does not hold, which would push that component as "other".
func TestComponentsCoverTheRegistry(t *testing.T) {
	have := map[string]bool{}
	for _, n := range accounting.Components {
		have[n] = true
	}
	names := cgcomponents.Names()
	if len(names) == 0 {
		t.Fatal("context-guru registered no components; the test cannot check the list")
	}
	for _, n := range names {
		if !have[n] {
			t.Errorf("context-guru component %q is not in accounting.Components", n)
		}
	}
	// And the operators Cortex's own envelope ops report.
	for _, n := range []string{"toolfilter", "toolschema", "headttl", "cachesplit", "expand", "adjudicate"} {
		if !have[n] {
			t.Errorf("envelope operator %q is not in accounting.Components", n)
		}
	}
}
