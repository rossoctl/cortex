package all_test

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins"
	_ "github.com/rossoctl/cortex/core/plugins/a2aparser"
	_ "github.com/rossoctl/cortex/core/plugins/contextguru"
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	_ "github.com/rossoctl/cortex/core/plugins/lineage"
	_ "github.com/rossoctl/cortex/core/plugins/mcpparser"
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

// A praxisOffSafe name that no plugin registers accepts nothing, and the real
// plugin is refused. Import a plugin here when you add it to the list. This test
// lives here, not in core/config, so the plugins' dependencies stay out of the
// module graph of every module that imports core/config.
func TestPraxisOffSafe_NamesRegisteredPlugins(t *testing.T) {
	registered := plugins.RegisteredPlugins()
	for _, name := range config.PraxisOffSafe() {
		if !slices.Contains(registered, name) {
			t.Errorf("praxisOffSafe has %q, which no plugin registers; registered: %v", name, registered)
		}
	}
}
