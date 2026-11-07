package inferencerouter

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// TestRouter_HostsAgreeWithWentTo holds the session's InferenceHosts table to wentTo. For random
// sessions with no archive, LastServer over the session's fold gives the same (server, ok) as
// wentTo's scan of the view, for each agent.
//
// The mutant rows change the table rule in one known way each. The test must find a session
// where the mutant and wentTo disagree. If it does not, the random sessions are too weak to
// hold the rule.
func TestRouter_HostsAgreeWithWentTo(t *testing.T) {
	r := New()
	if err := r.Configure(json.RawMessage(routerConfig(`"claude-code": "glm", "opencode": "ete"`))); err != nil {
		t.Fatal(err)
	}
	serverAt := func(h string) (string, bool) { s, ok := r.byHost[routerconfig.Key(h)]; return s, ok }

	cases := []struct {
		name   string
		folded func([]pipeline.SessionEvent) []pipeline.SessionEvent // the events the fold gets, nil for the view's
		table  func(pipeline.InferenceHosts)                         // a change to the folded table, nil for none
		agree  bool
	}{
		{name: "the rule", agree: true},
		{name: "mutant: aside turns are evidence", folded: withoutAsides},
		{name: "mutant: the table is keyed by AffinityName", folded: byAffinityName},
		{name: "mutant: LastUnrouted is ignored", table: func(h pipeline.InferenceHosts) {
			for i := range h {
				h[i].LastUnrouted = 0
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rnd := rand.New(rand.NewSource(7))
			disagreed := false
			for trial := 0; trial < 3000; trial++ {
				evs := randomSession(rnd)
				folded := evs
				if tc.folded != nil {
					folded = tc.folded(evs)
				}
				f := session.NewSummaryFold()
				for i := range folded {
					f.Add("s", &folded[i])
				}
				table, known := f.InferenceHosts()
				if !known {
					t.Fatalf("trial %d: a fold of new events cannot vouch for its table", trial)
				}
				if tc.table != nil {
					tc.table(table)
				}
				for _, agent := range []string{"claude-code", "opencode", "curl", ""} {
					ws, wok := r.wentTo(&pipeline.SessionView{ID: "s", Events: evs}, agent)
					gs, gok := table.LastServer(agent, serverAt)
					if ws == gs && wok == gok {
						continue
					}
					if tc.agree {
						t.Fatalf("trial %d agent %q: wentTo (%q, %v), table (%q, %v)\nevents %+v\ntable %+v",
							trial, agent, ws, wok, gs, gok, evs, table)
					}
					disagreed = true
				}
			}
			if !tc.agree && !disagreed {
				t.Fatal("no session tells the mutant from wentTo")
			}
		})
	}
}

// randomSession is up to 11 events. Each one is a request, a response, a denial or an inbound
// row, with or without an inference parse, from one of 4 clients, to a server host in one of
// two forms or to another host, with or without a captured path and a RequestedHost.
func randomSession(rnd *rand.Rand) []pipeline.SessionEvent {
	hosts := []string{eteHost, glmHost, "ETE.example.com:443", "GLM.example.com:8443", "opencode.ai", "api.anthropic.com", "127.0.0.1:11434"}
	uas := []string{claudeUA, opencodeUA, "curl/8.7.1", ""}
	paths := []string{"", "/v1/messages", "/zen/v1/chat/completions", "/v1/models", "/v1/messages/count_tokens"}
	roles := []pipeline.AgentRole{"", pipeline.AgentRoleMain, pipeline.AgentRoleSubagent}
	var evs []pipeline.SessionEvent
	for i := range rnd.Intn(12) {
		e := pipeline.SessionEvent{Seq: uint64(i + 1), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host: hosts[rnd.Intn(len(hosts))], Client: pipeline.ParseUserAgent(uas[rnd.Intn(len(uas))]),
			HTTPPath: paths[rnd.Intn(len(paths))]}
		if rnd.Intn(4) > 0 {
			e.HTTPMethod = http.MethodPost
		}
		if rnd.Intn(3) == 0 {
			e.RequestedHost = hosts[rnd.Intn(len(hosts))]
		}
		switch rnd.Intn(8) {
		case 0:
			e.Phase = pipeline.SessionResponse
		case 1:
			e.Phase = pipeline.SessionDenied
		case 2:
			e.Direction = pipeline.Inbound
		}
		if rnd.Intn(5) > 0 {
			ext := &pipeline.InferenceExtension{AgentRole: roles[rnd.Intn(len(roles))]}
			for range rnd.Intn(2) {
				ext.Tools = append(ext.Tools, pipeline.InferenceTool{Name: "T"})
			}
			if rnd.Intn(2) == 0 {
				ext.Messages = append(ext.Messages, pipeline.InferenceMessage{Role: "assistant"})
			}
			e.Inference = ext
		}
		evs = append(evs, e)
	}
	return evs
}

// byAffinityName is evs with each client named by its AffinityName alone, so that the fold keys
// the table by AffinityName and not by AgentName(Label()).
func byAffinityName(evs []pipeline.SessionEvent) []pipeline.SessionEvent {
	out := make([]pipeline.SessionEvent, len(evs))
	for i, e := range evs {
		e.Client = &pipeline.EventClient{Name: e.Client.AffinityName()}
		out[i] = e
	}
	return out
}

// withoutAsides is evs with each aside turn's role removed, so that session.TurnOf no longer
// calls it an aside and the fold takes it as evidence.
func withoutAsides(evs []pipeline.SessionEvent) []pipeline.SessionEvent {
	out := make([]pipeline.SessionEvent, len(evs))
	for i, e := range evs {
		if session.TurnOf(e.Inference) == turnAside {
			ext := *e.Inference
			ext.AgentRole = ""
			e.Inference = &ext
		}
		out[i] = e
	}
	return out
}
