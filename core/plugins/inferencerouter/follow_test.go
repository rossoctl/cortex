package inferencerouter

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
)

// renamedConfig is routerConfig with glm renamed zai: the same host, its own key so a
// test can tell which server's key a request carries, and the agents block given.
func renamedConfig(agents string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {` + agents + `}
	}`
}

// memPin is session's pin as the process store holds it, failing when there is none.
func memPin(t *testing.T, store *memstore.Store, session string) pin {
	t.Helper()
	v, ok := store.Get(pinPrefix + session)
	if !ok {
		t.Fatalf("session %s has no pin", session)
	}
	pn, ok := v.(pin)
	if !ok {
		t.Fatalf("pin for %s is a %T, want a pin", session, v)
	}
	return pn
}

// pinnedToGLM routes session s1 of claude-code to glm through a router configured with
// routerConfig, so its pin names glm and keeps glm's host.
func pinnedToGLM(t *testing.T, store *memstore.Store) {
	t.Helper()
	first := request(store, eteHost, claudeUA, "s1")
	run(t, build(t, routerConfig(`"claude-code": "glm"`)), first)
	assertRouted(t, first, glmHost, "glm-key")
	if pn := memPin(t, store, "s1"); pn.server != "glm" || pn.host != "glm.example.com" {
		t.Fatalf("pin = %#v, want glm on glm.example.com", pn)
	}
}

func TestDecidePin(t *testing.T) {
	// The config: zai is on glm's old host, ete on its own, and glm now on another.
	servers := map[string]string{"ete": "ete.example.com", "zai": "glm.example.com", "glm": "new.example.com"}
	hostOf := func(name string) (string, bool) { h, ok := servers[name]; return h, ok }
	serverAt := func(host string) (string, bool) {
		for name, h := range servers {
			if h == host {
				return name, true
			}
		}
		return "", false
	}
	for _, tc := range []struct {
		name    string
		had     pin
		want    pin
		changed bool
	}{
		{"not routed stays as it is",
			pin{agent: "a"}, pin{agent: "a"}, false},
		{"a pin on its server's host stays as it is",
			pin{agent: "a", server: "ete", host: "ete.example.com"}, pin{agent: "a", server: "ete", host: "ete.example.com"}, false},
		{"a renamed server is followed by its host",
			pin{agent: "a", server: "old", host: "ete.example.com"}, pin{agent: "a", server: "ete", host: "ete.example.com"}, true},
		{"the host wins over an old name given to another server",
			pin{agent: "a", server: "glm", host: "glm.example.com"}, pin{agent: "a", server: "zai", host: "glm.example.com"}, true},
		{"the host wins when two names are swapped",
			pin{agent: "a", server: "ete", host: "glm.example.com"}, pin{agent: "a", server: "zai", host: "glm.example.com"}, true},
		{"a URL replaced under the same name is followed by the name",
			pin{agent: "a", server: "glm", host: "gone.example.com"}, pin{agent: "a", server: "glm", host: "new.example.com"}, true},
		{"a pin with no host gets its server's",
			pin{agent: "a", server: "ete"}, pin{agent: "a", server: "ete", host: "ete.example.com"}, true},
		{"a pin with no host to a server gone stays for the 503",
			pin{agent: "a", server: "east"}, pin{agent: "a", server: "east"}, false},
		{"a pin whose host and name no server has stays for the 503",
			pin{agent: "a", server: "east", host: "east.example.com"}, pin{agent: "a", server: "east", host: "east.example.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := decidePin(tc.had, hostOf, serverAt)
			if got != tc.want || changed != tc.changed {
				t.Errorf("decidePin(%#v) = %#v, %v; want %#v, %v", tc.had, got, changed, tc.want, tc.changed)
			}
		})
	}
}

// The live case behind the rule: agentop server rename glm zai, or the same edit by
// hand. The running session stays on the server it was on, under its new name and with
// its key, and the record says which name it left.
func TestRouter_APinFollowsItsServerToANewName(t *testing.T) {
	store := newStore(t)
	pinnedToGLM(t, store)

	p := build(t, renamedConfig(`"claude-code": "ete"`))
	next := request(store, eteHost, claudeUA, "s1")
	run(t, p, next)
	assertRouted(t, next, glmHost, "zai-key")
	assertRecord(t, next, pipeline.ActionModify, "routed",
		map[string]string{"server": "zai", "pin": pinExisting, "renamed_from": "glm"})
	if pn := memPin(t, store, "s1"); pn.server != "zai" || pn.host != "glm.example.com" {
		t.Errorf("pin = %#v, want it to name zai now", pn)
	}

	// Followed once: the next request is an ordinary pinned one.
	again := request(store, eteHost, claudeUA, "s1")
	run(t, p, again)
	assertRouted(t, again, glmHost, "zai-key")
	if d := routerRecord(t, again).Details; d["renamed_from"] != "" {
		t.Errorf("the second request after the rename says renamed_from=%q, want none", d["renamed_from"])
	}
}

// A routed record names its server and pin, and a followed one the name it left; the
// turn that leaves an unpinned session unrouted is the unrouted record's alone. A
// one-shot is routed with turnAside, and its record says nothing of it.
func TestRouter_ARoutedOneShotsRecordCarriesNoTurn(t *testing.T) {
	store := newStore(t)
	pctx := claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 0, 0)
	run(t, build(t, routerConfig(`"claude-code": "glm"`)), pctx)
	assertRouted(t, pctx, glmHost, "glm-key")
	if d := routerRecord(t, pctx).Details; d["turn"] != "" || d["renamed_from"] != "" {
		t.Errorf("record details = %v, want server and pin only", d)
	}
}

// A renamed server's old name given to another server must not take the session
// there: the host decides, so it stays on zai.
func TestRouter_APinFollowsItsHostWhenItsOldNameIsReused(t *testing.T) {
	store := newStore(t)
	pinnedToGLM(t, store)

	reused := build(t, `{
		"servers": {
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "https://other.example.com", "key": "other-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {"claude-code": "glm"}
	}`)
	next := request(store, eteHost, claudeUA, "s1")
	run(t, reused, next)
	assertRouted(t, next, glmHost, "zai-key")
	assertRecord(t, next, pipeline.ActionModify, "routed", map[string]string{"server": "zai", "renamed_from": "glm"})
}

// agentop server add --yes replacing a server's URL says the sessions on it use the new
// one from their next request: with no server on the pin's host, the name decides, and
// the pin takes the new host. It was not renamed, so the record says nothing of it.
func TestRouter_APinFollowsANameWhoseURLChanged(t *testing.T) {
	store := newStore(t)
	pinnedToGLM(t, store)

	moved := build(t, `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "https://glm2.example.com", "key": "glm2-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {"claude-code": "ete"}
	}`)
	next := request(store, eteHost, claudeUA, "s1")
	run(t, moved, next)
	assertRouted(t, next, "glm2.example.com", "glm2-key")
	if d := routerRecord(t, next).Details; d["server"] != "glm" || d["renamed_from"] != "" {
		t.Errorf("record details = %v, want server glm and no renamed_from", d)
	}
	if pn := memPin(t, store, "s1"); pn.host != "glm2.example.com" {
		t.Errorf("pin = %#v, want glm's new host", pn)
	}
}

// A pin saved before pins kept a host gets its server's on its next request, so a
// rename after that keeps the session. One renamed before that request still gets the
// 503 a removed server's does: nothing says where its server went.
func TestRouter_APinWithNoHostGetsItsServersOnItsNextRequest(t *testing.T) {
	store := newStore(t)
	store.Put(pinPrefix+"s1", pin{agent: "claude-code", server: "glm"}, pinTTL)
	store.Put(pinPrefix+"s2", pin{agent: "claude-code", server: "glm"}, pinTTL)

	first := request(store, eteHost, claudeUA, "s1")
	run(t, build(t, routerConfig(`"claude-code": "ete"`)), first)
	assertRouted(t, first, glmHost, "glm-key")
	if pn := memPin(t, store, "s1"); pn.host != "glm.example.com" {
		t.Fatalf("pin = %#v, want glm's host filled in", pn)
	}

	renamed := build(t, renamedConfig(`"claude-code": "ete"`))
	s1 := request(store, eteHost, claudeUA, "s1")
	run(t, renamed, s1)
	assertRouted(t, s1, glmHost, "zai-key")

	s2 := request(store, eteHost, claudeUA, "s2")
	if a := run(t, renamed, s2); a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want the 503 for a pin with no host to a name no server has", a)
	}
	assertRecord(t, s2, pipeline.ActionDeny, "pinned_server_removed", map[string]string{"server": "glm"})
}

// A pin is its agent's: another agent's request filed under the session neither
// follows nor rewrites it, and the owner's next request follows it.
func TestRouter_AnotherAgentsRequestDoesNotFollowThePin(t *testing.T) {
	store := newStore(t)
	pinnedToGLM(t, store)

	p := build(t, renamedConfig(`"claude-code": "ete", "opencode": "ete"`))
	other := request(store, eteHost, opencodeUA, "s1")
	run(t, p, other)
	assertRouted(t, other, eteHost, "ete-key")
	if pn := memPin(t, store, "s1"); pn.server != "glm" {
		t.Errorf("pin = %#v after another agent's request, want glm untouched", pn)
	}

	owner := request(store, eteHost, claudeUA, "s1")
	run(t, p, owner)
	assertRouted(t, owner, glmHost, "zai-key")
}

// agentop server rename rewrites a sessions: entry naming the old name with the key, so
// the entry names the server the pin follows to: the session stays put, not moved from
// the old name.
func TestRouter_AnOverrideToTheNewNameKeepsThePin(t *testing.T) {
	store := newStore(t)
	pinnedToGLM(t, store)

	p := build(t, `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {"claude-code": "ete"},
		"sessions": {"s1": "zai"}
	}`)
	next := request(store, eteHost, claudeUA, "s1")
	run(t, p, next)
	assertRouted(t, next, glmHost, "zai-key")
	assertRecord(t, next, pipeline.ActionModify, "routed",
		map[string]string{"server": "zai", "pin": pinExisting, "renamed_from": "glm"})
}

// The reloader builds a fresh router from the renamed file: the pin in the process
// store the listener keeps follows the rename.
func TestPinsFollowARenameAcrossAReload(t *testing.T) {
	store := newStore(t)
	run(t, reloadBuild(t, routerConfig(`"claude-code": "glm"`)), request(store, eteHost, claudeUA, "s1"))

	next := request(store, eteHost, claudeUA, "s1")
	run(t, reloadBuild(t, renamedConfig(`"claude-code": "ete"`)), next)
	assertRouted(t, next, glmHost, "zai-key")
}

// A session given a substitute on glm keeps it on zai: the router does not re-pick a
// model for a conversation part-way, rename or no rename. zai's list has a newer GLM,
// which a re-pick would choose.
func TestRouter_AFollowedPinKeepsItsSubstitute(t *testing.T) {
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	first := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, first)
	resend(p, first, http.StatusForbidden, refusal)

	r := New()
	if err := r.Configure(json.RawMessage(`{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "glm", "helper": "nemotron"}
		},
		"agents": {"opencode": "zai"}
	}`)); err != nil {
		t.Fatal(err)
	}
	r.catalogs.set("zai", append([]string{"rits/zai-org/glm-6"}, glmModels...))
	renamed, err := pipeline.New([]pipeline.Plugin{r})
	if err != nil {
		t.Fatal(err)
	}
	next := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, renamed, next)
	assertRouted(t, next, glmHost, "zai-key")
	if got := sentModel(t, next); got != glmMain {
		t.Errorf("model = %q, want the substitute the session was given on glm", got)
	}
}

// A moved session's record names the server that wrote each reasoning block. Renamed
// with it, the blocks glm wrote are still read as the destination's own, and none is
// dropped.
func TestRouter_AFollowedSessionKeepsItsOwnReasoning(t *testing.T) {
	store := newStore(t)
	onGLM := sessionsConfig(`"claude-code": "ete"`, `"s1": "glm"`)
	send(t, store, onGLM, history())
	send(t, store, onGLM, history(thinkingTurn("g1")))

	onZai := `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {"claude-code": "ete"},
		"sessions": {"s1": "zai"}
	}`
	next := send(t, store, onZai, history(thinkingTurn("g1"), thinkingTurn("g2")))
	assertSignatures(t, next, "g1", "g2")
	assertNoInvocation(t, next, "reasoning_stripped")
}

// With the plugin store a local install has, a stored pin keeps its server's host, and
// a rename made while the proxy was stopped is followed after it starts again.
func TestRouter_WithAStoreAPinFollowsARenameMadeWhileStopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-state.json")
	before := openStore(t, path)
	run(t, buildWithStore(t, routerConfig(`"claude-code": "glm"`), before, nil),
		request(newStore(t), eteHost, claudeUA, "s1"))
	if rec, _ := storedPin(t, before, "s1"); rec.Server != "glm" || rec.Host != "glm.example.com" {
		t.Fatalf("stored pin = %+v, want glm with its host", rec)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after := openStore(t, path)
	next := request(newStore(t), eteHost, claudeUA, "s1")
	run(t, buildWithStore(t, renamedConfig(`"claude-code": "ete"`), after, nil), next)
	assertRouted(t, next, glmHost, "zai-key")
	if rec, _ := storedPin(t, after, "s1"); rec.Server != "zai" || rec.Host != "glm.example.com" {
		t.Errorf("stored pin = %+v, want it to name zai now", rec)
	}
}

// A stored pin is renewed at most once a day, but one with no host gets its server's on
// its next request whenever that is: a rename the next day must find it.
func TestRouter_AStoredPinWithNoHostGetsOneOnItsNextRequest(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "plugin-state.json"))
	now := time.Unix(1_800_000_000, 0)
	raw, _ := json.Marshal(pinRecord{Agent: "claude-code", Server: "glm", Renewed: now.Unix()})
	if err := st.Set(t.Context(), pinPrefix+"s1", string(raw), pinTTL); err != nil {
		t.Fatal(err)
	}
	run(t, buildWithStore(t, routerConfig(`"claude-code": "ete"`), st, func() time.Time { return now }),
		request(newStore(t), eteHost, claudeUA, "s1"))
	if rec, _ := storedPin(t, st, "s1"); rec.Host != "glm.example.com" {
		t.Errorf("stored pin = %+v, want glm's host filled in despite the once-a-day renewal", rec)
	}
}
