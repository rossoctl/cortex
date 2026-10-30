package tui

import (
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

func TestPluginDepsAllSatisfied_RequiresMet(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "a", Direction: "outbound", Position: 1},
		{Name: "b", Direction: "outbound", Position: 2, Requires: []string{"a"}},
	}
	if !pluginDepsAllSatisfied(&chain[1], chain) {
		t.Fatal("Requires=['a'] should be satisfied when 'a' is at position 1")
	}
}

func TestPluginDepsAllSatisfied_RequiresMissing(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "b", Direction: "outbound", Position: 1, Requires: []string{"a"}},
	}
	if pluginDepsAllSatisfied(&chain[0], chain) {
		t.Fatal("Requires=['a'] should NOT be satisfied when 'a' is absent")
	}
}

func TestPluginDepsAllSatisfied_RequiresMisordered(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "b", Direction: "outbound", Position: 1, Requires: []string{"a"}},
		{Name: "a", Direction: "outbound", Position: 2},
	}
	if pluginDepsAllSatisfied(&chain[0], chain) {
		t.Fatal("Requires=['a'] should NOT be satisfied when 'a' is downstream")
	}
}

func TestPluginDepsAllSatisfied_RequiresAnyMet(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "b", Direction: "outbound", Position: 1},
		{Name: "c", Direction: "outbound", Position: 2,
			RequiresAny: []string{"a", "b"}},
	}
	if !pluginDepsAllSatisfied(&chain[1], chain) {
		t.Fatal("RequiresAny should be satisfied when one alternative is upstream")
	}
}

func TestPluginDepsAllSatisfied_RequiresAnyAllMissing(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "c", Direction: "outbound", Position: 1,
			RequiresAny: []string{"a", "b"}},
	}
	if pluginDepsAllSatisfied(&chain[0], chain) {
		t.Fatal("RequiresAny should NOT be satisfied when no alternative is present")
	}
}

func TestPluginHasAnyDeps(t *testing.T) {
	if pluginHasAnyDeps(&apiclient.PipelinePlugin{Name: "x"}) {
		t.Fatal("plugin with no deps should report false")
	}
	if !pluginHasAnyDeps(&apiclient.PipelinePlugin{Name: "x", Requires: []string{"a"}}) {
		t.Fatal("plugin with Requires should report true")
	}
	if !pluginHasAnyDeps(&apiclient.PipelinePlugin{Name: "x", RequiresAny: []string{"a"}}) {
		t.Fatal("plugin with RequiresAny should report true")
	}
}

func TestUnmetDepsCount_Zero(t *testing.T) {
	m := &model{
		pipeline: &apiclient.PipelineView{
			Inbound: []apiclient.PipelinePlugin{
				{Name: "a", Direction: "inbound", Position: 1},
				{Name: "b", Direction: "inbound", Position: 2, Requires: []string{"a"}},
			},
		},
	}
	if got := m.unmetDepsCount(); got != 0 {
		t.Fatalf("unmetDepsCount = %d, want 0", got)
	}
}

func TestUnmetDepsCount_Two(t *testing.T) {
	m := &model{
		pipeline: &apiclient.PipelineView{
			Inbound: []apiclient.PipelinePlugin{
				{Name: "needy-a", Direction: "inbound", Position: 1, Requires: []string{"missing"}},
			},
			Outbound: []apiclient.PipelinePlugin{
				{Name: "needy-b", Direction: "outbound", Position: 1, RequiresAny: []string{"x"}},
			},
		},
	}
	if got := m.unmetDepsCount(); got != 2 {
		t.Fatalf("unmetDepsCount = %d, want 2", got)
	}
}

// TestPluginDepsAllSatisfied_RequiresAnyMisorderOnAlternate locks the
// stricter semantics: when one RequiresAny target is upstream (good)
// but ANOTHER named target is downstream (misorder), the overall
// result is NOT satisfied. Earlier this incorrectly returned true.
func TestPluginDepsAllSatisfied_RequiresAnyMisorderOnAlternate(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "a", Direction: "outbound", Position: 1},
		{Name: "c", Direction: "outbound", Position: 2,
			RequiresAny: []string{"a", "b"}},
		{Name: "b", Direction: "outbound", Position: 3},
	}
	if pluginDepsAllSatisfied(&chain[1], chain) {
		t.Fatal("RequiresAny should NOT be satisfied when an alternative is downstream")
	}
}

// The #1568 case in the DEPS indicator: a RequiresLater misorder used to
// render ✓ (and still pass the pre-apply check), while the proxy refused
// the reload. The verdict now comes from edit.CheckOrder, the same copy
// of the framework's rules the pre-apply validator runs.
func TestPluginDepsAllSatisfied_RequiresLaterMet(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "litellm-budget-track", Direction: "outbound", Position: 1,
			RequiresLater: []string{"inference-parser"}},
		{Name: "inference-parser", Direction: "outbound", Position: 2},
	}
	if !pluginDepsAllSatisfied(&chain[0], chain) {
		t.Fatal("RequiresLater should be satisfied when the target is downstream")
	}
	if !pluginHasAnyDeps(&chain[0]) {
		t.Fatal("RequiresLater alone should mark the plugin as having deps")
	}
}

func TestPluginDepsAllSatisfied_RequiresLaterMisordered(t *testing.T) {
	// The #1568 case: the parser moved above the plugin that reads its
	// response-phase output.
	chain := []apiclient.PipelinePlugin{
		{Name: "inference-parser", Direction: "outbound", Position: 1},
		{Name: "litellm-budget-track", Direction: "outbound", Position: 2,
			RequiresLater: []string{"inference-parser"}},
	}
	if pluginDepsAllSatisfied(&chain[1], chain) {
		t.Fatal("RequiresLater should NOT be satisfied when the target is upstream")
	}
}

func TestPluginDepsAllSatisfied_RequiresLaterMissing(t *testing.T) {
	chain := []apiclient.PipelinePlugin{
		{Name: "litellm-budget-track", Direction: "outbound", Position: 1,
			RequiresLater: []string{"inference-parser"}},
	}
	if pluginDepsAllSatisfied(&chain[0], chain) {
		t.Fatal("RequiresLater should NOT be satisfied when the target is absent")
	}
}
