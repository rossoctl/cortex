package tui

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// activeRouter is the config of the inference-router the proxy runs, read off /v1/pipeline, and
// whether it routes. on is false wherever routerStatus has a reason it does not: the SERVER
// columns hide, and S refuses with that reason.
func (m *model) activeRouter() (c routerconfig.Config, on bool) {
	c, why := m.routerStatus()
	return c, why == ""
}

// routerStatus is the inference-router entry of the outbound pipeline the proxy runs, and why it
// routes nothing — "" when it routes. The reason is S's refusal, so it names this machine's
// config file, m.localConfigPath, where the fix goes, as `agentop server` does (homeTilde); S
// asks only when attached to it.
//
// THE RUNNING CONFIGURATION, NOT THE FILE: what the SERVER columns and the S picker show is
// what the proxy routes by now, and an edit it has not reloaded — or refused — is not that. The
// keys arrive as "[REDACTED]" (core/redact blanks every field named key), and nothing here
// reads one.
//
// ITS POLICY AS WELL AS ITS CONFIG: under on_error: observe the router stays in the pipeline
// and keeps its config, but moves nothing, so a column read off the config alone would name a
// server no request goes to, and S would write a route that does not happen. servers.Inactive
// decides it, as it does for `agentop server`, and words it the same way.
//
// Decoded leniently, as `agentop server`'s listing reads the file: the proxy validated this
// config before it ran it. But a config that does not decode at all is not on — a column read
// off half of one would name servers that were never read — and that is the one reason here no
// edit to the file fixes, since the proxy runs it: an agentop older than the proxy reading a
// field it does not know the shape of.
func (m *model) routerStatus() (c routerconfig.Config, why string) {
	if m.pipeline == nil {
		return routerconfig.Config{}, "agentop has not read this Cortex's pipeline yet; try again in a moment"
	}
	for _, p := range m.pipeline.Outbound {
		if p.Name != servers.PluginName {
			continue
		}
		if why := servers.Inactive(p.OnError, homeTilde(m.localConfigPath)); why != "" {
			return routerconfig.Config{}, why
		}
		if err := json.Unmarshal(p.Config, &c); err != nil {
			return routerconfig.Config{}, "agentop cannot read the inference-router's running config, so it can neither show nor change its routing; " +
				"an agentop as new as the proxy can"
		}
		// The same canonical fold the router keys its table with, so the SERVER
		// columns and the S picker name the agent the proxy routes for it — a config
		// written before an agent-name rename otherwise shows no server for an agent
		// that has one. See readRouter, the file-reading twin of this fold.
		c.Agents = pipeline.FoldAgentNames(c.Agents)
		return c, ""
	}
	// NOT CONFIGURED, OR OFF, AND THIS SIDE CANNOT TELL WHICH: the proxy does not build an
	// on_error: off plugin, so /v1/pipeline lists neither. Both fixes, then.
	return routerconfig.Config{}, "this Cortex runs no inference-router: add a server with agentop server add <name> <url>, " +
		"or, if " + homeTilde(m.localConfigPath) + " has one under on_error: off, remove on_error to route"
}

// homeTilde is path as agentop's commands print it: under the home directory, as ~/…. S's
// reasons are in the words `agentop server` prints (servers.Inactive), which names the file this
// way, so the two say the same thing about the same file. package main's homeTilde is the same
// rule; the subcommands live there, which this package cannot import.
//
// Reads $HOME and nothing under it, so a test points it with t.Setenv.
func homeTilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}

// sameRouter reports whether two router configs route alike, so a pipeline refetch repaints the
// SERVER columns only when what they show has changed.
func sameRouter(a, b routerconfig.Config) bool {
	// DeepEqual: a Server holds maps (api, headers), so it is not comparable.
	return maps.EqualFunc(a.Servers, b.Servers, func(x, y routerconfig.Server) bool { return reflect.DeepEqual(x, y) }) &&
		maps.Equal(a.Agents, b.Agents)
}

// localCortexTarget is the config file and stats URL of the Cortex on this machine, when the
// endpoint on screen IS that Cortex. pipelineStore's local half and S share it, so `e` and S
// cannot disagree about which proxy they would change.
//
// Compared against localEndpoint rather than localEndpointOr(): the fallback is the in-cluster
// 9094, and matching that would claim a hand-run port-forward to a POD is this machine's config.
func (m *model) localCortexTarget() (path, statsURL string, ok bool) {
	if m.client != nil && m.localEndpoint != "" && sameEndpoint(m.client.Endpoint(), m.localEndpoint) &&
		m.localConfigPath != "" && m.localStatsURL != "" {
		return m.localConfigPath, m.localStatsURL, true
	}
	return "", "", false
}

// agentOwnChoice is the AGENTS pane's SERVER cell for an agent the router does not route:
// each of its sessions goes where its key sends it (key_routes:, else the key's server).
//
// THE WORDING IS THE DIMMING. The spec draws it dimmed, and a table cell can carry no ANSI —
// the table truncates escapes as text (TestSessionsRows_CarryNoANSIUnderAForcedColourProfile).
// No server can be named this, since a server name has no space, so it never reads as one.
const agentOwnChoice = "by key"

// agentsServerWidth fits agentOwnChoice and every server name worth typing.
const agentsServerWidth = 12

// agentServerCell is the AGENTS pane's SERVER cell for the row labelled label: the server its
// new sessions go to, or agentOwnChoice. A row no server can be chosen for shows nothing: Other,
// which pools every agent Cortex does not recognise, so there is no one agent for the router to
// name, and any agent the router refuses to route — the no-User-Agent row, whose requests belong
// to no one agent. "by key" there would offer a choice S then refuses.
//
// SANITISED AT RENDER TIME, as every served label is: the proxy only runs server names its rule
// allows, but the name arrives over an unauthenticated API, and nothing here assumes the producer
// checked anything.
func agentServerCell(router routerconfig.Config, label string) string {
	agent := pipeline.AgentName(label)
	if label == otherAgents || routerconfig.CheckAgent(agent) != nil {
		return ""
	}
	if s := router.Agents[agent]; s != "" {
		return sanitizeLabel(s)
	}
	return agentOwnChoice
}

// sessionsServerWidth is the sessions table's SERVER column: a server name, or a host shown in
// parentheses and cut to fit.
const sessionsServerWidth = 12

// sessionsNotAHost is the SERVER cell for a session whose inference host is not one a hostname
// can spell. Nothing of it is drawn: see sessionServerCell. The space is what keeps it from
// reading as a host called that, as agentOwnChoice's does for a server.
const sessionsNotAHost = "(not a host)"

// sessionServerCell is the sessions table's SERVER cell for session s: the session's server,
// from the server name when the proxy sent one, else from the host its inference traffic last
// went to (servers.ForSession); a host no server has, in parentheses; or emptyCell when the
// session has no inference traffic.
//
// PARENTHESES ARE THE DIMMING, for agentOwnChoice's reason. They are not in a server name's
// alphabet, so "(api.anthropic.com)" cannot read as a server called that.
//
// ONLY A HOST IS EVER DRAWN, and only one that is one. inferenceHost is the host[:port] the
// request carried — client-controlled, served over an unauthenticated API — so a value holding
// anything a hostname cannot (an '@' with a key before it, a path or query, a control, or a
// parenthesis that would draw one host as two) shows sessionsNotAHost instead, and nothing of
// its text. Splitting off the port is not enough: routerconfig.Hostname splits at the last ':',
// so "sk-…:x@evil.example" comes back as "sk-…", the key (servers.Host refuses a URL whole for
// the same reason). That alphabet is
// stricter than sanitizeLabel, so what passes it needs no sanitising; a server name does.
func sessionServerCell(router routerconfig.Config, s session.SessionSummary, w int) string {
	host := s.InferenceHost
	if host == "" {
		return emptyCell
	}
	if name, ok := servers.ForSession(router, s); ok {
		return trunc(sanitizeLabel(name), w)
	}
	h := routerconfig.Hostname(host)
	if h == "" || !isHostPort(host) {
		return trunc(sessionsNotAHost, w)
	}
	return trunc("("+h+")", w)
}

// isHostPort reports whether s is spelled only from a hostname's letters, digits, '.', '-' and
// '_', plus the ':' and brackets of a port and an IPv6 literal.
func isHostPort(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// sessionsShowServer reports whether the sessions table carries SERVER: when the router the
// proxy runs routes — activeRouter's rule, so an observing router hides it here as on the agents
// pane — and has more than one server. With one there is nothing for the column to tell apart.
func (m *model) sessionsShowServer() (routerconfig.Config, bool) {
	router, on := m.activeRouter()
	return router, on && len(router.Servers) > 1
}
