package all_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
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

// TestBodyReaderPlugins_ImportsEveryPlugin fails when a plugins_<name>.go file
// imports a plugin that this file does not, so that the catalog above sees every
// plugin. cpex is left out: it needs the cpex tag, and it declares both body writes.
func TestBodyReaderPlugins_ImportsEveryPlugin(t *testing.T) {
	imports := func(path string) []string {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range f.Imports {
			p, _ := strconv.Unquote(s.Path.Value)
			out = append(out, p)
		}
		return out
	}
	here := imports("bodyreaders_test.go")
	files, err := filepath.Glob("plugins_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no plugins_*.go files (%v)", err)
	}
	for _, f := range files {
		for _, p := range imports(f) {
			if p != "github.com/rossoctl/cortex/core/plugins/cpex" && !slices.Contains(here, p) {
				t.Errorf("%s imports %s; import it in bodyreaders_test.go too", f, p)
			}
		}
	}
}
