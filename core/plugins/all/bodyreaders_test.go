package all_test

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins"
	_ "github.com/rossoctl/cortex/core/plugins/a2aparser"
	_ "github.com/rossoctl/cortex/core/plugins/contextguru"
	_ "github.com/rossoctl/cortex/core/plugins/evolve"
	_ "github.com/rossoctl/cortex/core/plugins/ibac"
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	_ "github.com/rossoctl/cortex/core/plugins/inferencerouter"
	_ "github.com/rossoctl/cortex/core/plugins/jwtvalidation"
	_ "github.com/rossoctl/cortex/core/plugins/lineage"
	_ "github.com/rossoctl/cortex/core/plugins/litellm_budgettrack"
	_ "github.com/rossoctl/cortex/core/plugins/mcpparser"
	_ "github.com/rossoctl/cortex/core/plugins/opa"
	_ "github.com/rossoctl/cortex/core/plugins/script"
	_ "github.com/rossoctl/cortex/core/plugins/sessionbudget"
	_ "github.com/rossoctl/cortex/core/plugins/sparc"
	_ "github.com/rossoctl/cortex/core/plugins/staticinject"
	_ "github.com/rossoctl/cortex/core/plugins/tokenbroker"
	_ "github.com/rossoctl/cortex/core/plugins/tokenexchange"
	_ "github.com/rossoctl/cortex/core/plugins/tokenexchangeaiac"
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

// config.BodyReaderPlugins is a copy of what the registry declares, so that
// agentop can check the plugin order without linking the plugins. This test
// imports every plugin of core/plugins/all except cpex (it needs the cpex tag).
// Import a new plugin here when you add it to core/plugins/all.
func TestBodyReaderPlugins_MatchesTheCatalog(t *testing.T) {
	var want []string
	for _, e := range plugins.Catalog() {
		c := e.Capabilities
		if c.ReadsBody && !c.WritesRequestBody && !c.WritesResponseBody {
			want = append(want, e.Name)
		}
	}
	slices.Sort(want)
	got := config.BodyReaderPlugins()
	if !slices.Equal(want, got) {
		t.Errorf("config.BodyReaderPlugins() = %v; the catalog's body readers are %v", got, want)
	}
	registered := plugins.RegisteredPlugins()
	for _, name := range got {
		if !slices.Contains(registered, name) {
			t.Errorf("BodyReaderPlugins has %q, which no plugin registers; registered: %v", name, registered)
		}
	}
}
