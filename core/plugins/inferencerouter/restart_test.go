package inferencerouter

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// hydrate gives pctx the store's view and facts for its session, as the forward proxy's
// hydrateSession does: an ID-only view when the store has no entry.
func hydrate(st *session.Store, pctx *pipeline.Context) *pipeline.Context {
	v, f := st.ViewFacts(pctx.SessionID)
	if v == nil {
		v = &pipeline.SessionView{ID: pctx.SessionID}
	}
	pctx.Session = v
	pctx.SetSessionFacts(f)
	return pctx
}

// outcome is what the router did with one request: where it went, the session's pin, and the
// record's turn detail.
type outcome struct {
	host, pin, turn string
	pinned          bool
}

func outcomeOf(t *testing.T, pctx *pipeline.Context, pins *memstore.Store) outcome {
	t.Helper()
	pin, pinned := pinOf(t, pins, pctx.SessionID)
	return outcome{host: pctx.Host, pin: pin, turn: routerRecord(t, pctx).Details["turn"], pinned: pinned}
}

// zenRow is an OpenCode request row to host, which the client sent to requested ("" when it
// was not redirected).
func zenRow(requested, host string) pipeline.SessionEvent {
	e := sent(host, opencodeUA)
	e.HTTPMethod, e.HTTPPath, e.RequestedHost = http.MethodPost, "/zen/v1/chat/completions", requested
	return e
}

// tunnelRow is the bridged CONNECT's row: no evidence.
func tunnelRow() pipeline.SessionEvent {
	return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.anthropic.com:443",
		Tunnel: true, HTTPMethod: http.MethodConnect, Client: pipeline.ParseUserAgent(claudeUA)}
}

// continuation is a Claude Code continuation, sent to api.anthropic.com: with no evidence the
// router leaves it there.
func continuation(pins pipeline.SharedStore, id string) *pipeline.Context {
	return claudeCode(request(pins, "api.anthropic.com", claudeUA, id), pipeline.AgentRoleMain, 26, 3)
}

func openArchive(t *testing.T, dir string) *archive.Archive {
	t.Helper()
	a, err := archive.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A restart does not change the router's answer (plan §3.7). For each case of §3.6 rows 4-9, the
// same request gets the same server, pin and turn:
//
//	A: before the restart, from the store's facts.
//	B: after it, before the session has an entry: the archive's table.
//	C: after an event with no evidence creates the entry: the entry merged with the archive's table.
//	D: after it, with no facts: today's scan of the view and the archive.
func TestRouter_TheAnswerIsTheSameAcrossARestart(t *testing.T) {
	const id = "running"
	claude := claudeCodeOnGLM
	opencode := routerConfig(`"opencode": "glm"`)
	redirected := sent(glmHost, claudeUA)
	redirected.RequestedHost = eteHost
	zen := func(pins pipeline.SharedStore, id string) *pipeline.Context {
		pctx := request(pins, "opencode.ai", opencodeUA, id)
		pctx.Path = "/zen/v1/chat/completions"
		return pctx
	}
	cases := []struct {
		name    string
		config  string
		history func(t *testing.T) []pipeline.SessionEvent
		ask     func(pins pipeline.SharedStore, id string) *pipeline.Context
		want    outcome
	}{
		{"4: the latest row to a server, in case and default-port form", claude,
			func(*testing.T) []pipeline.SessionEvent {
				return []pipeline.SessionEvent{sent(glmHost, claudeUA), sent("ETE.example.com:443", claudeUA)}
			},
			continuation, outcome{host: eteHost, pin: "ete", pinned: true}},
		{"5: a row with no method or path, then an older server row", opencode,
			func(*testing.T) []pipeline.SessionEvent {
				return []pipeline.SessionEvent{sent(eteHost, opencodeUA), sent("opencode.ai", opencodeUA)}
			},
			zen, outcome{host: eteHost, pin: "ete", pinned: true}},
		{"6: ran unrouted on its provider", opencode,
			func(*testing.T) []pipeline.SessionEvent { return []pipeline.SessionEvent{zenRow("", "opencode.ai")} },
			zen, outcome{host: "opencode.ai", pinned: true}},
		{"7: captured to a server", opencode,
			func(*testing.T) []pipeline.SessionEvent {
				return []pipeline.SessionEvent{zenRow("opencode.ai", eteHost)}
			},
			zen, outcome{host: eteHost, pin: "ete", pinned: true}},
		{"8: the server the row went to, not the one it asked for", routerConfig(`"claude-code": "ete"`),
			func(*testing.T) []pipeline.SessionEvent { return []pipeline.SessionEvent{redirected} },
			continuation, outcome{host: glmHost, pin: "glm", pinned: true}},
		{"9: an opening turn the router sent to glm, then side rows and a tunnel", claude,
			func(t *testing.T) []pipeline.SessionEvent {
				open := claudeCode(request(newStore(t), "api.anthropic.com", claudeUA, id), pipeline.AgentRoleMain, 26, 0)
				run(t, build(t, claude), open)
				if open.Host != glmHost {
					t.Fatalf("the opening turn went to %q, want glm", open.Host)
				}
				oneShot := claudeCode(request(nil, eteHost, claudeUA, id), pipeline.AgentRoleMain, 0, 0)
				subagent := claudeCode(request(nil, eteHost, claudeUA, id), pipeline.AgentRoleSubagent, 11, 2)
				return []pipeline.SessionEvent{recorded(open), recorded(oneShot), recorded(subagent), tunnelRow()}
			},
			continuation, outcome{host: glmHost, pin: "glm", pinned: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ask runs the request through p with a new pin store: the plugin state is lost.
			ask := func(p *pipeline.Pipeline, prepare func(*pipeline.Context)) outcome {
				pins := newStore(t)
				pctx := tc.ask(pins, id)
				prepare(pctx)
				run(t, p, pctx)
				return outcomeOf(t, pctx, pins)
			}
			dir := t.TempDir()
			a1 := openArchive(t, dir)
			st1 := session.New(0, 0, 0)
			st1.AddRecorder(a1)
			for _, e := range tc.history(t) {
				st1.Append(id, e)
			}
			a := ask(build(t, tc.config), func(pctx *pipeline.Context) { hydrate(st1, pctx) })
			if a != tc.want {
				t.Fatalf("A, before the restart: %+v, want %+v", a, tc.want)
			}
			if err := a1.Close(); err != nil {
				t.Fatal(err)
			}
			st1.Close()

			a2 := openArchive(t, dir)
			t.Cleanup(func() { _ = a2.Close() })
			st2 := session.New(0, 0, 0)
			t.Cleanup(st2.Close)
			st2.AddRecorder(a2)
			if b := ask(build(t, tc.config), func(pctx *pipeline.Context) { hydrate(st2, pctx) }); b != a {
				t.Errorf("B, no entry: %+v, want A %+v", b, a)
			}
			st2.Append(id, tunnelRow())
			if c := ask(build(t, tc.config), func(pctx *pipeline.Context) { hydrate(st2, pctx) }); c != a {
				t.Errorf("C, an entry after the history: %+v, want A %+v", c, a)
			}
			d := ask(buildWithHistory(t, tc.config, a2), func(pctx *pipeline.Context) { pctx.Session = st2.View(id) })
			if d != a {
				t.Errorf("D, today's scan: %+v, want A %+v", d, a)
			}
		})
	}
}

// The G rows of plan §3.6 are an accepted change: where today's scan misses older evidence that the
// table keeps, the table's answer holds the session on the server its agent last used. Each test
// shows the old answer on the old path (no facts), then the new one.
var (
	claudeCodeOnGLM = routerConfig(`"claude-code": "glm"`)
	stayedPut       = outcome{host: "api.anthropic.com", turn: turnContinuation, pinned: true}
	onGLM           = outcome{host: glmHost, pin: "glm", pinned: true}
)

// G1: with max_events set, the store pins the A2A intent, and the archive read starts below it.
// Evidence between the pin and the tail that a trim removed is in neither read.
func TestRouter_EvidenceBetweenThePinnedIntentAndTheTailIsKept(t *testing.T) {
	intent := pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest,
		A2A: &pipeline.A2AExtension{Method: "message/send"}}
	events := []pipeline.SessionEvent{intent, sent(glmHost, claudeUA), tunnelRow(), tunnelRow(), tunnelRow()}
	st := session.New(0, 3, 0)
	defer st.Close()
	for _, e := range events {
		st.Append("s1", e)
	}
	h := &fakeHistory{events: map[string][]pipeline.SessionEvent{"s1": archived(events...)}}
	if v := st.View("s1"); len(v.Events) != 3 || v.Events[0].A2A == nil {
		t.Fatalf("fixture: the view is %+v, want the pinned intent and the last 2 rows", v.Events)
	}

	pins := newStore(t)
	old := continuation(pins, "s1")
	old.Session = st.View("s1")
	run(t, buildWithHistory(t, claudeCodeOnGLM, h), old)
	if got := outcomeOf(t, old, pins); got != stayedPut {
		t.Errorf("old path: %+v, want %+v", got, stayedPut)
	}

	pins = newStore(t)
	pctx := hydrate(st, continuation(pins, "s1"))
	run(t, buildWithHistory(t, claudeCodeOnGLM, h), pctx)
	if got := outcomeOf(t, pctx, pins); got != onGLM {
		t.Errorf("table: %+v, want %+v", got, onGLM)
	}
}

// G3: after a restart, evidence more than historyLimit archived events back is past today's
// search, and it is in the archive's table.
func TestRouter_EvidencePastTheArchiveSearchLimitIsKept(t *testing.T) {
	dir := t.TempDir()
	a1 := openArchive(t, dir)
	st1 := session.New(0, 0, 0)
	st1.AddRecorder(a1)
	st1.Append("s1", sent(glmHost, claudeUA))
	for range historyLimit {
		st1.Append("s1", sent(glmHost, opencodeUA))
	}
	if err := a1.Close(); err != nil {
		t.Fatal(err)
	}
	st1.Close()
	a2 := openArchive(t, dir)
	defer func() { _ = a2.Close() }()
	st2 := session.New(0, 0, 0)
	defer st2.Close()
	st2.AddRecorder(a2)

	pins := newStore(t)
	old := continuation(pins, "s1")
	run(t, buildWithHistory(t, claudeCodeOnGLM, a2), old)
	if got := outcomeOf(t, old, pins); got != stayedPut {
		t.Errorf("old path: %+v, want %+v", got, stayedPut)
	}

	pins = newStore(t)
	pctx := hydrate(st2, continuation(pins, "s1"))
	run(t, buildWithHistory(t, claudeCodeOnGLM, a2), pctx)
	if got := outcomeOf(t, pctx, pins); got != onGLM {
		t.Errorf("table: %+v, want %+v", got, onGLM)
	}
}

// G4: with max_events set and no archive, evidence a trim removed is not in the view, and it is
// in the entry's table: a trim sheds nothing from it.
func TestRouter_EvidenceATrimRemovedIsKept(t *testing.T) {
	st := session.New(0, 2, 0)
	defer st.Close()
	for _, e := range []pipeline.SessionEvent{sent(glmHost, claudeUA), tunnelRow(), tunnelRow()} {
		st.Append("s1", e)
	}

	pins := newStore(t)
	old := continuation(pins, "s1")
	old.Session = st.View("s1")
	run(t, build(t, claudeCodeOnGLM), old)
	if got := outcomeOf(t, old, pins); got != stayedPut {
		t.Errorf("old path: %+v, want %+v", got, stayedPut)
	}

	pins = newStore(t)
	pctx := hydrate(st, continuation(pins, "s1"))
	run(t, build(t, claudeCodeOnGLM), pctx)
	if got := outcomeOf(t, pctx, pins); got != onGLM {
		t.Errorf("table: %+v, want %+v", got, onGLM)
	}
}

// G7: an archive read that fails is no evidence today. With the table, the router reads no
// archive at all.
func TestRouter_WithTheTableTheArchiveIsNotRead(t *testing.T) {
	table := pipeline.SessionFacts{InferenceHosts: pipeline.InferenceHosts{{Agent: "claude-code", Host: glmHost, Last: 1}},
		HostsKnown: true}
	tunnel := tunnelRow()
	tunnel.Seq = 2
	ask := func(facts bool) (outcome, *fakeHistory) {
		h := &fakeHistory{err: errors.New("segment unreadable")}
		pins := newStore(t)
		pctx := withHistory(continuation(pins, "s1"), tunnel)
		if facts {
			pctx.SetSessionFacts(table)
		}
		run(t, buildWithHistory(t, claudeCodeOnGLM, h), pctx)
		return outcomeOf(t, pctx, pins), h
	}
	if got, _ := ask(false); got != stayedPut {
		t.Errorf("old path: %+v, want %+v", got, stayedPut)
	}
	got, h := ask(true)
	if got != onGLM {
		t.Errorf("table: %+v, want %+v", got, onGLM)
	}
	if len(h.befores) != 0 {
		t.Errorf("archive read %d times with the table, want none", len(h.befores))
	}
}

// The table holds hosts, not server names, so a hot reload that renames a server maps the session
// to the server on its host now (§3.6 row 15).
func TestRouter_TheTableIsMappedByTheServersOfTheCurrentConfig(t *testing.T) {
	renamed := `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"zai": {"url": "https://glm.example.com:8443", "key": "zai-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {"claude-code": "ete"}
	}`
	pins := newStore(t)
	pctx := continuation(pins, "s1")
	pctx.SetSessionFacts(pipeline.SessionFacts{InferenceHosts: pipeline.InferenceHosts{{Agent: "claude-code", Host: glmHost, Last: 1}},
		HostsKnown: true})
	run(t, build(t, renamed), pctx)
	assertRouted(t, pctx, glmHost, "zai-key")
	if pin, _ := pinOf(t, pins, "s1"); pin != "zai" {
		t.Errorf("pin = %q, want zai, the server on the table's host now", pin)
	}
}

// §3.6 row 1: a context with a session id and no view has no session to vouch for, so a known
// table that the listener set is not evidence. The request stays where it is.
func TestRouter_ATableWithNoSessionViewIsNoEvidence(t *testing.T) {
	pins := newStore(t)
	pctx := continuation(pins, "s1")
	pctx.Session = nil
	pctx.SetSessionFacts(pipeline.SessionFacts{InferenceHosts: pipeline.InferenceHosts{{Agent: "claude-code", Host: glmHost, Last: 1}},
		HostsKnown: true})
	run(t, build(t, claudeCodeOnGLM), pctx)
	if got := outcomeOf(t, pctx, pins); got != stayedPut {
		t.Errorf("%+v, want %+v", got, stayedPut)
	}
}
