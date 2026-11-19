package inferencerouter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// entries is the outbound chain a config file with inference-parser and the router
// would build.
func entries(routerConfig string) []config.PluginEntry {
	return []config.PluginEntry{{Name: "inference-parser"}, {Name: Name, Config: json.RawMessage(routerConfig)}}
}

// reloadBuild builds the chain the way cmd/cortex does on startup and on every
// reload: through the registry, for a listener that honors redirects.
func reloadBuild(t *testing.T, routerConfig string) *pipeline.Pipeline {
	t.Helper()
	p, err := plugins.BuildWithDeps(entries(routerConfig),
		plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	return p
}

// TestPinsSurviveAReload is the claim switching rests on. The reloader builds a new
// pipeline from the edited file and swaps it in; the process store the listener
// holds is not rebuilt, so the new router instance reads the old one's pins. A
// running session keeps its server, routed or not, and only a new session sees the
// edit.
func TestPinsSurviveAReload(t *testing.T) {
	store := newStore(t)
	before := reloadBuild(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "routed"))
	run(t, before, request(store, eteHost, opencodeUA, "unrouted"))

	after := reloadBuild(t, routerConfig(`"claude-code": "ete", "opencode": "glm"`))

	routed := request(store, eteHost, claudeUA, "routed")
	run(t, after, routed)
	assertRouted(t, routed, glmHost, "glm-key")

	unrouted := request(store, eteHost, opencodeUA, "unrouted")
	run(t, after, unrouted)
	assertUntouched(t, unrouted, eteHost)

	fresh := request(store, eteHost, opencodeUA, "fresh")
	run(t, after, fresh)
	assertRouted(t, fresh, glmHost, "glm-key")
}

// A reload from one server to two on one URL is accepted.
func TestReload_SharedHost(t *testing.T) {
	store := newStore(t)
	run(t, reloadBuild(t, routerConfig(`"claude-code": "glm"`)), request(store, eteHost, claudeUA, "s1"))
	after := reloadBuild(t, sharedConfig(`"claude-code": "team-b"`))
	fresh := request(store, gwHost, claudeUA, "s2")
	run(t, after, fresh)
	assertRouted(t, fresh, gwHost, "team-b-key")
}

// A key_routes: entry to a server that is not listed is refused on reload too.
func TestReload_RefusesUnknownKeyRoute(t *testing.T) {
	_, err := plugins.BuildWithDeps(entries(keyConfig("", `"team-b": "nope"`)),
		plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err == nil || !strings.Contains(err.Error(), "key_routes.team-b") {
		t.Fatalf("err = %v, want it to name key_routes.team-b", err)
	}
}
