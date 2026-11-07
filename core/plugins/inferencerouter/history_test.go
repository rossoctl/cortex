package inferencerouter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/session"
)

// fakeHistory is a session archive: each session's events in the order they were
// recorded. It notes the before of every read and how many events it handed out.
type fakeHistory struct {
	events  map[string][]pipeline.SessionEvent
	err     error
	befores []uint64
	read    int
}

func (h *fakeHistory) Earlier(id string, before uint64, fn func(*pipeline.SessionEvent) bool) error {
	h.befores = append(h.befores, before)
	if h.err != nil {
		return h.err
	}
	evs := h.events[id]
	for i := len(evs) - 1; i >= 0; i-- {
		if before > 0 && evs[i].Seq >= before {
			continue
		}
		h.read++
		if !fn(&evs[i]) {
			return nil
		}
	}
	return nil
}

// archived numbers events from seq 1 as the archive holds them.
func archived(events ...pipeline.SessionEvent) []pipeline.SessionEvent {
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	return events
}

// buildWithHistory is build with h injected before Configure, as plugins.BuildWithDeps does.
func buildWithHistory(t *testing.T, config string, h session.History) *pipeline.Pipeline {
	t.Helper()
	r := New()
	r.SetHistory(h)
	if err := r.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{r})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// The restart that first keeps pins on disk starts with none, and a restart with the
// plugin state lost does the same: the proxy's memory holds nothing of a running
// session either. Its archived events still say where it went, and that history keeps
// a Claude Code continuation on glm rather than leaving it where Claude Code sends it.
// Another agent's later row, filed under the session, is no evidence of its server.
func TestRouter_ASessionsArchivedHistoryKeepsItsServerAcrossARestart(t *testing.T) {
	h := &fakeHistory{events: map[string][]pipeline.SessionEvent{
		"running": archived(sent(glmHost, claudeUA), sent(eteHost, "curl/8.7.1")),
	}}
	store := newStore(t)
	p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), h)
	pctx := claudeCode(request(store, eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
	if pin, _ := pinOf(t, store, "running"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}

// Memory is read first, and the archive only below the oldest event memory holds:
// above it the archive holds the same events, which memory already showed are no
// evidence.
func TestRouter_TheArchiveIsReadBelowTheOldestEventInMemory(t *testing.T) {
	tunnel := pipeline.SessionEvent{Seq: 5, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: eteHost + ":443", Tunnel: true, HTTPMethod: http.MethodConnect, Client: pipeline.ParseUserAgent(claudeUA)}
	h := &fakeHistory{events: map[string][]pipeline.SessionEvent{
		"running": archived(sent(glmHost, claudeUA), sent(eteHost, claudeUA), sent(glmHost, claudeUA), sent(eteHost, opencodeUA), tunnel),
	}}
	p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), h)
	pctx := withHistory(claudeCode(request(newStore(t), eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3), tunnel)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	if len(h.befores) != 1 || h.befores[0] != 5 {
		t.Errorf("archive read with before = %v, want one read below seq 5", h.befores)
	}
}

// What memory shows is not read again from the archive.
func TestRouter_HistoryInMemoryNeedsNoArchiveRead(t *testing.T) {
	h := &fakeHistory{}
	p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), h)
	pctx := withHistory(request(newStore(t), eteHost, claudeUA, "running"), sent(eteHost, claudeUA))
	run(t, p, pctx)

	assertRouted(t, pctx, eteHost, "ete-key")
	if len(h.befores) != 0 {
		t.Errorf("archive read %d times, want none", len(h.befores))
	}
}

// With nothing in the archive to go on — no session, no request of this agent's to a
// server, or a read that failed — the request's own turn decides, as without an
// archive.
func TestRouter_WithNoArchivedEvidenceTheTurnDecides(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    *fakeHistory
	}{
		{"nothing archived", &fakeHistory{}},
		{"only another agent's request", &fakeHistory{events: map[string][]pipeline.SessionEvent{
			"running": archived(sent(glmHost, opencodeUA)),
		}}},
		{"a failed read", &fakeHistory{err: errors.New("segment unreadable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), tc.h)
			pctx := claudeCode(request(newStore(t), eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3)
			run(t, p, pctx)

			assertUntouched(t, pctx, eteHost)
			assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNew, "turn": turnContinuation})
		})
	}
}

// The archive is read on the request path, so the search stops after historyLimit
// events; past it the request's own turn decides.
func TestRouter_TheArchiveSearchStopsAtItsLimit(t *testing.T) {
	events := []pipeline.SessionEvent{sent(glmHost, claudeUA)}
	for range historyLimit {
		events = append(events, sent(glmHost, opencodeUA))
	}
	h := &fakeHistory{events: map[string][]pipeline.SessionEvent{"running": archived(events...)}}
	p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), h)
	pctx := claudeCode(request(newStore(t), eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3)
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	if h.read != historyLimit {
		t.Errorf("read %d archived events, want %d", h.read, historyLimit)
	}
}

// A request no parser read as inference — Claude Code's count_tokens, a /v1/models
// list — says nothing about where a conversation is, so it pins nothing: it goes to
// its agent's current server, and the session's next inference request decides.
// Claude Code sends count_tokens mid-conversation, and as the first request after an
// idle gap.
func TestRouter_ARequestWithNoInferenceParseDecidesNothing(t *testing.T) {
	unparsed := func(store pipeline.SharedStore, ua string) *pipeline.Context {
		pctx := request(store, eteHost, ua, "s1")
		pctx.Path, pctx.Extensions.Inference = "/v1/messages/count_tokens", nil
		return pctx
	}

	t.Run("a Claude Code continuation after it is still one", func(t *testing.T) {
		store := newStore(t)
		p := build(t, routerConfig(`"claude-code": "glm"`))
		first := unparsed(store, claudeUA)
		run(t, p, first)
		assertRouted(t, first, glmHost, "glm-key")
		assertRecord(t, first, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNone})
		if _, pinned := pinOf(t, store, "s1"); pinned {
			t.Fatal("an unparsed request pinned the session")
		}

		next := claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 3)
		run(t, p, next)
		assertUntouched(t, next, eteHost)
		assertRecord(t, next, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNew, "turn": turnContinuation})
	})

	t.Run("another agent's session starts on the server it was on at its first inference request", func(t *testing.T) {
		store := newStore(t)
		run(t, build(t, routerConfig(`"opencode": "glm"`)), unparsed(store, opencodeUA))
		if _, pinned := pinOf(t, store, "s1"); pinned {
			t.Fatal("an unparsed request pinned the session")
		}

		next := request(store, eteHost, opencodeUA, "s1")
		run(t, build(t, routerConfig(`"opencode": "ete"`)), next)
		assertRouted(t, next, eteHost, "ete-key")
		assertRecord(t, next, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinNew})
	})

	t.Run("history still decides", func(t *testing.T) {
		store := newStore(t)
		p := build(t, routerConfig(`"claude-code": "glm"`))
		pctx := withHistory(unparsed(store, claudeUA), sent(eteHost, claudeUA))
		run(t, p, pctx)
		assertRouted(t, pctx, eteHost, "ete-key")
		if pin, _ := pinOf(t, store, "s1"); pin != "ete" {
			t.Errorf("pin = %q, want ete", pin)
		}
	})
}

// The router decides from inference-parser's record of each request, so a chain
// without the parser before it is refused rather than run unable to pin a session.
func TestRouter_RequiresInferenceParserBeforeIt(t *testing.T) {
	_, err := plugins.BuildWithDeps([]config.PluginEntry{{Name: Name, Config: json.RawMessage(routerConfig(``))}},
		plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err == nil || !strings.Contains(err.Error(), `requires "inference-parser" earlier in the chain`) {
		t.Fatalf("err = %v, want the router refused for want of inference-parser", err)
	}
}

// A side request decides nothing, and so its row is no evidence of where the session
// is, in memory or in the archive: the conversation's own requests decide. A row of
// the conversation's own, older than the side requests, still does.
func TestRouter_ASideRequestsRowIsNoEvidence(t *testing.T) {
	oneShot := claudeCode(request(nil, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 0, 0)
	subagent := claudeCode(request(nil, eteHost, claudeUA, "s1"), pipeline.AgentRoleSubagent, 11, 2)
	turn := claudeCode(request(nil, glmHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 1)
	for _, tc := range []struct {
		name            string
		memory, archive []pipeline.SessionEvent
		turns           int    // assistant turns in the request
		host            string // where it goes
		pin             string
	}{
		{"opening turn after side rows in memory", []pipeline.SessionEvent{recorded(oneShot), recorded(subagent)}, nil, 0, glmHost, "glm"},
		{"opening turn after side rows in the archive", nil, []pipeline.SessionEvent{recorded(oneShot), recorded(subagent)}, 0, glmHost, "glm"},
		{"continuation after side rows only", nil, []pipeline.SessionEvent{recorded(subagent), recorded(oneShot)}, 3, eteHost, ""},
		{"continuation whose own turn is older than the side rows", nil,
			[]pipeline.SessionEvent{recorded(turn), recorded(oneShot), recorded(subagent)}, 3, glmHost, "glm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			h := &fakeHistory{events: map[string][]pipeline.SessionEvent{"s1": archived(tc.archive...)}}
			p := buildWithHistory(t, routerConfig(`"claude-code": "glm"`), h)
			pctx := withHistory(claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, tc.turns),
				tc.memory...)
			run(t, p, pctx)

			if pctx.Host != tc.host {
				t.Errorf("Host = %q, want %q", pctx.Host, tc.host)
			}
			if pin, pinned := pinOf(t, store, "s1"); !pinned || pin != tc.pin {
				t.Errorf("pin = %q (pinned %v), want %q", pin, pinned, tc.pin)
			}
		})
	}
}
