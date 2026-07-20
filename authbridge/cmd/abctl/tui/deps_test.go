package tui

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
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
