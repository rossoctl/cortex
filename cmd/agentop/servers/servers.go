// Package servers is what agentop's `server` subcommand and its TUI share about the
// inference-router: the entry's name, the check every write passes, how a server is shown, and
// which server a host belongs to.
//
// A package of its own because the subcommands live in package main, which the tui package
// cannot import, and because the generic writer in edit must stay generic: it knows nothing about
// any plugin, so the router's rules cannot live there. Two copies of Verify would be two answers
// to "may agentop write this", and the proxy only accepts one.
package servers

import (
	"fmt"
	"maps"
	"slices"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// PluginName is the inference-router plugin's registered name: the outbound entry every server
// write reads and changes.
const PluginName = "inference-router"

// Verify is the check every write of the router's config makes of its result: the router entry
// must pass the plugin's own validation, so agentop never writes a config the proxy will refuse
// to reload. It is edit.ConfigWrite.Verify for every caller.
func Verify(cfg *config.Config) error {
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name == PluginName {
			if _, err := routerconfig.Decode(e.Config); err != nil {
				return fmt.Errorf("%s config: %w", PluginName, err)
			}
		}
	}
	return nil
}

// Inactive is why a router entry under policy, its on_error, routes nothing, or "" when it
// routes. Under observe the router redirects nothing and records observe/would_route where it
// would have routed; under off it does not run. Either way a server given to an agent changes no
// traffic, and whatever said the agent's sessions now go there would be wrong. file is the config
// file as the caller shows it, for the fix; whether the result is "" does not depend on it.
//
// One wording for `agentop server`, which reads the policy off the file, and the TUI's S, which
// reads it off /v1/pipeline, so the two say the same thing about the same router.
func Inactive(policy pipeline.ErrorPolicy, file string) string {
	fix := fmt.Sprintf("Remove on_error from the entry in %s, or set it to enforce, to route.", file)
	switch policy.Resolved() {
	case pipeline.ErrorPolicyObserve:
		return fmt.Sprintf("The %s entry runs under on_error: observe, so nothing is routed: a request it would "+
			"route is only recorded, as observe/would_route. %s", PluginName, fix)
	case pipeline.ErrorPolicyOff:
		return fmt.Sprintf("The %s entry runs under on_error: off, so it does not run and nothing is routed. %s", PluginName, fix)
	}
	return ""
}

// AgentChange routes agent's new sessions to server, or stops routing it when server is "": the
// one change `agentop server use`, `agentop server reset` and the TUI's S picker make.
func AgentChange(agent, server string) edit.ConfigChange {
	ch := edit.ConfigChange{Chain: "outbound", Plugin: PluginName, Path: []string{"agents", agent}}
	if server != "" {
		ch.Value = edit.ScalarValue(server)
	}
	return ch
}

// Host is how a server's URL is shown: its host, or the whole URL for plain http, so a server
// whose traffic crosses the network unencrypted says so.
//
// A URL the router refuses shows nothing of itself, not even its host: the likeliest reason it
// is refused is a pasted key, and a key given as a username that contains '/', '?' or '#' ends
// the authority early, so url.Parse takes the key for the host (https://sk-.../x@gw.example
// parses with Host "sk-..."). No parentheses either: `server add` already puts this in some.
func Host(s routerconfig.Server) string {
	ep, err := routerconfig.ParseURL(s.URL)
	if err != nil {
		return "not a valid URL"
	}
	if ep.Scheme == "http" {
		return ep.URL()
	}
	return ep.Host
}

// Mapping is a server's main and helper words in one phrase, as the config gives
// them; what each resolves to is the server's list's to say.
func Mapping(s routerconfig.Server) string {
	return "main " + s.Main + " · helper " + s.Helper
}

// ForHost is the server whose host hostport names, compared as the plugin matches a request:
// port-stripped and lowercased. ok is false for a host no server has, and for "".
//
// Unambiguous because the router refuses two servers on one host — the rule exists so that a
// session's server can be named from where its requests went.
func ForHost(c routerconfig.Config, hostport string) (name string, ok bool) {
	if hostport == "" {
		return "", false
	}
	h := routerconfig.Hostname(hostport)
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		if ep, err := routerconfig.ParseURL(c.Servers[name].URL); err == nil && ep.Hostname == h {
			return name, true
		}
	}
	return "", false
}
