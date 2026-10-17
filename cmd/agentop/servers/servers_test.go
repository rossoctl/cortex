package servers

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

var twoServers = routerconfig.Config{Servers: map[string]routerconfig.Server{
	"ete": {URL: "https://ete.example.com", Key: "k", Main: "opus", Helper: "haiku"},
	"glm": {URL: "https://GLM.example.com:8443", Key: "k", Main: "glm", Helper: "nemotron"},
}}

func TestMapping(t *testing.T) {
	if got := Mapping(routerconfig.Server{Main: "glm", Helper: "nemotron"}); got != "main glm · helper nemotron" {
		t.Errorf("Mapping = %q, want main glm · helper nemotron", got)
	}
}

func TestHost(t *testing.T) {
	for url, want := range map[string]string{
		"https://ete.example.com:443":  "ete.example.com",
		"https://glm.example.com:8443": "glm.example.com:8443",
		"http://localhost:4000":        "http://localhost:4000",
	} {
		if got := Host(routerconfig.Server{URL: url}); got != want {
			t.Errorf("Host(%q) = %q, want %q", url, got, want)
		}
	}
}

// A URL the router refuses shows nothing of itself, not even its host: the likeliest
// reason it is refused is a pasted key, and a key given as a username containing
// '/', '?' or '#' ends the authority early, so url.Parse takes the key for the host.
func TestHost_ShowsNothingOfARefusedURL(t *testing.T) {
	for _, raw := range []string{
		"https://u:sk-secret@x.example",
		"https://sk-SECRET/x@h.example.com",
		"https://sk-SECRET?x@h.example.com",
		"https://sk-SECRET#x@h.example.com",
		"https://h.example.com/?key=sk-SECRET",
		"https://h.example.com:sk-SECRET",
	} {
		got := Host(routerconfig.Server{URL: raw})
		if got != "not a valid URL" {
			t.Errorf("Host(%q) = %q, want %q", raw, got, "not a valid URL")
		}
		parts := []string{"secret", "x.example", "h.example.com", "u:"}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			parts = append(parts, strings.ToLower(u.Host))
		}
		for _, part := range parts {
			if strings.Contains(strings.ToLower(got), part) {
				t.Errorf("Host(%q) = %q shows %q", raw, got, part)
			}
		}
	}
}

// Matched as the plugin matches a request: the port and the case do not matter, so an event
// recorded as glm.example.com:8443 and a server configured on GLM.example.com are one host.
func TestForHost(t *testing.T) {
	for host, want := range map[string]string{
		"ete.example.com":      "ete",
		"ete.example.com:443":  "ete",
		"glm.example.com:8443": "glm",
		"GLM.EXAMPLE.COM":      "glm",
		"api.anthropic.com":    "",
		"":                     "",
	} {
		got, ok := ForHost(twoServers, host)
		if got != want || ok != (want != "") {
			t.Errorf("ForHost(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestAgentChange(t *testing.T) {
	// PER-KEY, one change per spelling: a removal for every way the config might
	// name the agent, then the one set of the canonical key. A whole-map
	// replacement would depend on a snapshot of agents: — and revert a concurrent
	// write for another agent — so the changes touch nothing but this agent.
	set := AgentChanges("bob", "glm")
	if len(set) != 3 {
		t.Fatalf("AgentChanges(bob, glm) = %d changes, want 3 (two removals, one set): %+v", len(set), set)
	}
	for i, want := range []string{"bob-shell", "ibm-bob"} {
		if got := set[i]; got.Value != nil || strings.Join(got.Path, ".") != "agents."+want ||
			got.Chain != "outbound" || got.Plugin != PluginName || got.CreatePlugin {
			t.Errorf("removal %d = %+v, want agents.%s removed", i, got, want)
		}
	}
	if got := set[2]; strings.Join(got.Path, ".") != "agents.bob" || got.Value == nil || got.Value.Value != "glm" {
		t.Errorf("set = %+v, want agents.bob = glm", got)
	}
	// Stopping the routing removes the canonical spelling too, and only that: no
	// set, and nothing touching any other agent's keys.
	for _, got := range AgentChanges("claude-code", "") {
		if strings.Join(got.Path, ".") != "agents.claude-code" || got.Value != nil {
			t.Errorf("reset change = %+v, want only agents.claude-code removed", got)
		}
	}
}

// Verify applies the plugin's own rules to the router entry and ignores every other entry.
func TestVerify(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{
		{Name: "tool-prune", Config: []byte(`{"remove": []}`)},
		{Name: PluginName, Config: []byte(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k", "main": "opus", "helper": "haiku"}}, "agents": {"claude-code": "glm"}}`)},
	}
	if err := Verify(cfg); err == nil || !strings.Contains(err.Error(), `"glm" is not a server listed under servers`) {
		t.Errorf("Verify = %v, want the plugin's refusal of an agent routed to no server", err)
	}
	cfg.Pipeline.Outbound.Plugins[1].Config = []byte(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k", "main": "opus", "helper": "haiku"}}, "agents": {"claude-code": "ete"}}`)
	if err := Verify(cfg); err != nil {
		t.Errorf("Verify = %v on a valid router entry", err)
	}
}

// Inactive is "" exactly when an entry under policy routes — the default, spelled or not — and
// otherwise says why nothing is routed and how to fix it in file, which `agentop server` and the
// TUI's S both show.
func TestInactive(t *testing.T) {
	const file = "~/.cortex/config.yaml"
	for _, policy := range []pipeline.ErrorPolicy{"", pipeline.ErrorPolicyEnforce} {
		if got := Inactive(policy, file); got != "" {
			t.Errorf("Inactive(%q) = %q, want \"\"", policy, got)
		}
	}
	for policy, want := range map[pipeline.ErrorPolicy]string{
		pipeline.ErrorPolicyObserve: "The inference-router entry runs under on_error: observe, so nothing is routed: " +
			"a request it would route is only recorded, as observe/would_route. ",
		pipeline.ErrorPolicyOff: "The inference-router entry runs under on_error: off, so it does not run and nothing is routed. ",
	} {
		fix := "Remove on_error from the entry in " + file + ", or set it to enforce, to route."
		if got := Inactive(policy, file); got != want+fix {
			t.Errorf("Inactive(%q) =\n %q\nwant\n %q", policy, got, want+fix)
		}
	}
}

// On a loopback host a server is its host and port, as the plugin matches a request
// there: a session sent to another port on the same machine is on no server.
func TestForHost_LocalServersByPort(t *testing.T) {
	local := routerconfig.Config{Servers: map[string]routerconfig.Server{
		"ollama": {URL: "http://localhost:11434", Key: "k", Main: "g", Helper: "g"},
		"vllm":   {URL: "http://localhost:8000", Key: "k", Main: "q", Helper: "q"},
	}}
	for host, want := range map[string]string{
		"localhost:11434": "ollama",
		"localhost:8000":  "vllm",
		"localhost:3000":  "",
		"localhost":       "",
	} {
		got, ok := ForHost(local, host)
		if got != want || ok != (want != "") {
			t.Errorf("ForHost(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestSessionChanges(t *testing.T) {
	got := SessionChanges("s1", "glm")
	want := []edit.ConfigChange{{
		Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", "s1"},
		Value: edit.ScalarValue("glm"),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionChanges = %+v, want %+v", got, want)
	}
	// "" is a value, not a removal: the entry must stay, or the session's pin
	// would route it right back.
	got = SessionChanges("s1", "")
	want = []edit.ConfigChange{{
		Chain: "outbound", Plugin: PluginName, Path: []string{"sessions", "s1"},
		Value: edit.ScalarValue(""),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionChanges(\"\") = %+v, want %+v", got, want)
	}
}

func TestPruneSessions(t *testing.T) {
	cfg := routerconfig.Config{Sessions: map[string]string{
		"live": "glm", "dead": "glm", "archived": "ete",
	}}
	changes := PruneSessions(cfg, map[string]bool{"live": true, "archived": true})
	if len(changes) != 1 || changes[0].Path[1] != "dead" || changes[0].Value != nil {
		t.Errorf("PruneSessions = %+v, want one removal for dead", changes)
	}
	if changes := PruneSessions(cfg, nil); len(changes) != 0 {
		t.Errorf("PruneSessions with no live set = %+v, want nothing: prune only what you can see is dead", changes)
	}
}

// Verify refuses a sessions entry naming no server, through the same Decode the
// proxy runs — the write path and the proxy cannot disagree.
func TestVerify_RefusesASessionsEntryNamingNoServer(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{
		Name:   PluginName,
		Config: json.RawMessage(`{"servers":{"ete":{"url":"https://ete.example.com","key":"k","main":"opus","helper":"haiku"}},"sessions":{"s1":"nope"}}`),
	}}
	err := Verify(cfg)
	if err == nil || !strings.Contains(err.Error(), `sessions.s1: "nope" is not a server listed under servers`) {
		t.Fatalf("Verify error = %v, want it to refuse the unlisted server", err)
	}
}
