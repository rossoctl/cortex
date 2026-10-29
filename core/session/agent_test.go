package session

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

func agentOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	return summaryOf(t, s, id).Agent
}

func evFrom(c *pipeline.EventClient) pipeline.SessionEvent {
	e := ev()
	e.Client = c
	return e
}

// A session names the coding agent it belongs to by the label group=agent files that agent's
// traffic under: its affinity owner's when one claimed it, else the first known agent's. The
// first-wins half is what keeps a Claude Code session Claude's when a Bob call is filed into it
// later, and the owner half is what decides when the session's first event came from someone
// else.
func TestListSessions_NamesTheSessionsAgent(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	claudeNext := &pipeline.EventClient{Name: "claude-code", Version: "2.1.285"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}

	s.Append("claude-1", evFrom(nil))
	s.Append("claude-1", evFrom(claude))
	s.Append("claude-1", evFrom(bob))
	if got := agentOf(t, s, "claude-1"); got != "claude-code" {
		t.Errorf("claude-1 agent = %q, want claude-code (first known agent wins)", got)
	}
	s.Append("claude-2", evFrom(claudeNext))
	if got := agentOf(t, s, "claude-2"); got != "claude-code" {
		t.Errorf("claude-2 agent = %q, want claude-code: an agent's releases are one agent", got)
	}

	// Bob's header named task-1, so Bob owns it though Claude's call landed there first.
	s.Append("task-1", evFrom(claude))
	named := evFrom(bob)
	named.SessionHeader = BobSessionHeader
	s.Append("task-1", named)
	// The claim and the events both carry the pre-convergence spelling "bob-shell";
	// the read folds it to "bob", so a session claimed under the old name still
	// names its agent under the new one.
	if got := agentOf(t, s, "task-1"); got != "bob" {
		t.Errorf("task-1 agent = %q, want bob (the owner that claimed it)", got)
	}

	webFetch := &pipeline.EventClient{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"}
	s.Append("fetch-only", evFrom(webFetch))
	if got := agentOf(t, s, "fetch-only"); got != webFetch.Label() {
		t.Errorf("fetch-only agent = %q, want %q (AffinityName reads the comment)", got, webFetch.Label())
	}

	for _, id := range []string{DefaultSessionID, PendingSessionID("bob-shell")} {
		s.Append(id, evFrom(bob))
		if got := agentOf(t, s, id); got != "" {
			t.Errorf("%s agent = %q, want none: it is not one agent's session", id, got)
		}
	}
}

// An IBM Bob IDE session names its agent from the User-Agents the IDE really sends, the account
// call included. Unrecognised, these left the session's agent absent, so no AGENTS row could
// reach it (#1210).
func TestListSessions_NamesTheIBMBobIDE(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	for _, ua := range []string{
		"IBM Bob/2.2.1",
		"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1",
	} {
		s.Append("3981fd731b31c9d1aff4e17e6556f5c6", evFrom(pipeline.ParseUserAgent(ua)))
	}
	if got := agentOf(t, s, "3981fd731b31c9d1aff4e17e6556f5c6"); got != "bob" {
		t.Errorf("agent = %q, want bob", got)
	}
}

// The agent a session names is a key the usage ring's group=agent series has for the same
// events, so a client can match one against the other — a long version (folded away) included.
func TestListSessions_AgentIsTheGroupAgentLabel(t *testing.T) {
	clients := []*pipeline.EventClient{
		{Name: "claude-code", Version: "2.1.284"},
		{Name: "bob-shell", Version: strings.Repeat("9", 2*usage.MaxLabelLen)},
		{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"},
	}
	s := New(0, 0, 0)
	defer s.Close()
	agg := usage.New()
	for i, c := range clients {
		id := string(rune('a' + i))
		e := evFrom(c)
		e.Phase = pipeline.SessionResponse
		s.Append(id, e)
		agg.Record(id, &e)
	}
	series := usage.FoldSeriesAcrossWindow(agg.Snapshot(usage.BucketWidth, usage.BucketWidth, "", usage.GroupAgent).Buckets)
	for i := range clients {
		id := string(rune('a' + i))
		if got := agentOf(t, s, id); got == "" || series[got].Requests != 1 {
			t.Errorf("session %s agent = %q; group=agent has %v", id, got, slices.Sorted(maps.Keys(series)))
		}
	}
}

// A session from a client the proxy does NOT recognise still names the program it belongs to,
// under the same label group=agent files that client's traffic by (#1214). Before this, Append
// recorded an agent only when EventClient.AffinityName() was non-empty — empty for every
// User-Agent outside knownClients — so an IBM Bob IDE session came back with no `agent` at all
// and nothing in /v1/sessions said whose it was, while /v1/usage?group=agent had its spend under
// three raw labels.
//
// TABLE-DRIVEN OVER WHOLE SESSIONS rather than single events, because the rule being pinned is an
// ORDER over a session's clients — recognised first, then unrecognised, each first-wins — and no
// one-event case can show an order. Each case states which clients touched one session, in the
// order they did, and what the session must then be called.
func TestListSessions_NamesAnUnrecognisedAgent(t *testing.T) {
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	// The IBM Bob IDE's real User-Agents, from #1214's evidence. Parsed here rather than
	// hand-built so each case exercises the sanitise-and-cap path a request takes, and so it keeps
	// telling the truth when one of these tokens later joins knownClients: the wants are
	// usage.AgentLabel's answer for the same client, not literals.
	bobIDE := pipeline.ParseUserAgent("IBM Bob/2.2.1")
	bobSDK := pipeline.ParseUserAgent(
		"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1")
	// The IDE's update check, which knownClients deliberately does NOT claim ("code" is no
	// agent's token) — so this one is unrecognised today and designed to stay so.
	bobUpdate := pipeline.ParseUserAgent("Code/1.126.0+bob2.2.1 Darwin/24.6.0")
	curl := pipeline.ParseUserAgent("curl/8.4.0")

	cases := []struct {
		name    string
		id      string
		clients []*pipeline.EventClient
		want    string
		// why says what breaks if this case regresses, so a failure names the defect rather
		// than only the expectation.
		why string
	}{{
		name:    "recognised agent unchanged",
		id:      "claude-1",
		clients: []*pipeline.EventClient{claude},
		want:    "claude-code",
		// The field's existing contract. Widening it must not move a single session that
		// already named an agent.
		why: "a recognised agent still reports its canonical name, versionless",
	}, {
		name:    "unrecognised agent now labelled",
		id:      "bob-1",
		clients: []*pipeline.EventClient{bobUpdate},
		want:    usage.AgentLabel(bobUpdate),
		// #1214 itself: the session that used to come back bare.
		why: "an unrecognised client names the session by its raw User-Agent label",
	}, {
		name:    "recognised beats unrecognised whichever came first",
		id:      "mixed-1",
		clients: []*pipeline.EventClient{curl, claude},
		want:    "claude-code",
		// The reason the unrecognised pass runs SECOND. One first-wins order over both would
		// re-label a Claude Code session after whatever probe reached it first, which is a
		// regression on every session that works today.
		why: "a recognised agent outranks an unrecognised client that arrived earlier",
	}, {
		name:    "first unrecognised client wins among several",
		id:      "bob-2",
		clients: []*pipeline.EventClient{bobUpdate, curl},
		want:    usage.AgentLabel(bobUpdate),
		// One program sends several User-Agents (three, for this IDE). First-wins keeps the
		// session named by the client that named it rather than re-labelling it per turn.
		why: "first-wins applies to unrecognised clients too, not just recognised ones",
	}, {
		name:    "a recognised agent inside a comment is still recognised",
		id:      "sdk-1",
		clients: []*pipeline.EventClient{bobSDK},
		want:    "bob",
		// AffinityName is WIDER than Name: it scans later tokens, so this one is recognised and
		// takes the recognised pass. The case is here to pin that the new unrecognised pass did
		// not swallow it — keyed by label, it would have reported the whole SDK string.
		why: "AffinityName's trailing scan still decides, and reports the canonical name",
	}, {
		name: "no client at all stays bare",
		id:   "headless-1",
		// nil is NO User-Agent, which EventClient.Label calls "unknown" — a reserved bucket for
		// unattributed traffic, not a program. Rendering it here would put a value in the field a
		// client attributes a session by that no request ever sent, and `agent` is omitempty on
		// the standing rule that an unknown value must not render as a real one.
		clients: []*pipeline.EventClient{nil, nil},
		want:    "",
		why:     "a session with nothing to attribute still omits agent",
	}, {
		name: "unrecognised client after a header-less one",
		id:   "headless-2",
		// The "unknown" bucket must not block the real label behind it: nothing is recorded for
		// the nil client, so the curl event is still the session's first entry.
		clients: []*pipeline.EventClient{nil, curl},
		want:    usage.AgentLabel(curl),
		why:     "an absent User-Agent records nothing and does not shadow a later labelled client",
	}, {
		name: "default bucket stays bare",
		id:   DefaultSessionID,
		// The default and pending buckets are no single agent's — denial events and several
		// agents' header-less calls aggregate there — so widening the field must not name them.
		// agentop depends on it: inOtherAgents routes a bucket into the Other row precisely
		// because it names no recognised agent.
		clients: []*pipeline.EventClient{bobIDE, curl},
		want:    "",
		why:     "the default bucket is no one agent's and must keep no agent",
	}, {
		name:    "pending bucket stays bare",
		id:      PendingSessionID("bob-shell"),
		clients: []*pipeline.EventClient{bobIDE, curl},
		want:    "",
		why:     "a pending bucket is no one agent's and must keep no agent",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(0, 0, 0)
			defer s.Close()
			for _, c := range tc.clients {
				s.Append(tc.id, evFrom(c))
			}
			if got := agentOf(t, s, tc.id); got != tc.want {
				t.Errorf("agent = %q, want %q: %s", got, tc.want, tc.why)
			}
		})
	}
}

// An unrecognised agent's session and its usage row carry the SAME string, which is the whole
// point of #1214: a client matches one against the other instead of spending a request per
// session on /v1/usage?session=, which covers only the ring's six hours. Asserted against the
// ring rather than against literals, because a literal would agree with a label one of the two
// sides capped or sanitised differently.
func TestListSessions_UnrecognisedAgentMatchesTheGroupAgentLabel(t *testing.T) {
	uas := []string{
		"Code/1.126.0+bob2.2.1 Darwin/24.6.0",
		"appconfiguration-node-sdk",
		// Longer than usage.MaxLabelLen, so the cap is exercised on both sides. #1214's evidence
		// has exactly this case: a label the ring had already cut at 96 bytes.
		"unrecognised-sdk/3.0.36 runtime/node.js/v24.15.0 very-long-agent/" + strings.Repeat("9", 2*usage.MaxLabelLen),
	}
	s := New(0, 0, 0)
	defer s.Close()
	agg := usage.New()
	for i, ua := range uas {
		id := "u" + string(rune('a'+i))
		e := evFrom(pipeline.ParseUserAgent(ua))
		e.Phase = pipeline.SessionResponse
		s.Append(id, e)
		agg.Record(id, &e)
	}
	series := usage.FoldSeriesAcrossWindow(agg.Snapshot(usage.BucketWidth, usage.BucketWidth, "", usage.GroupAgent).Buckets)
	for i := range uas {
		id := "u" + string(rune('a'+i))
		got := agentOf(t, s, id)
		if got == "" {
			t.Errorf("session %s names no agent; %q should have labelled it", id, uas[i])
			continue
		}
		if series[got].Requests != 1 {
			t.Errorf("session %s agent = %q, absent from group=agent %v", id, got, slices.Sorted(maps.Keys(series)))
		}
		if len(got) > usage.MaxLabelLen {
			t.Errorf("session %s agent is %d bytes, over usage.MaxLabelLen (%d): %q",
				id, len(got), usage.MaxLabelLen, got)
		}
	}
}

// The label is CLIENT-ASSERTED and spoofable, so it must reach the field already sanitised and
// capped — by usage.AgentLabel, the same choke point the usage ring's byAgent keys and the
// ledger's rows pass through, rather than by a rule invented in this package. /v1/sessions is
// unauthenticated and its rows reach a terminal, where U+009B alone opens a CSI sequence
// (CWE-150) and a bidi override makes one agent's label read as another's.
func TestListSessions_SanitisesAnUnrecognisedAgentsLabel(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	// A C1 CSI, a bidi override and a zero-width space, none of which ParseUserAgent's own
	// tab-to-space normalisation touches.
	s.Append("hostile", evFrom(pipeline.ParseUserAgent("evil-agent/12J‮edoc-edualc​")))
	got := agentOf(t, s, "hostile")
	if got == "" {
		t.Fatalf("agent is empty; a hostile User-Agent is still a label")
	}
	for _, r := range got {
		if pipeline.IsControlRune(r) {
			t.Errorf("agent %q carries display-rewriting rune %U; usage.AgentLabel should have replaced it", got, r)
		}
	}
}

// The archive's fold answers what the store does for an unrecognised client, and keeps doing so
// across the session.json round-trip a restart makes. foldAgent gained an `unknown` flag for
// this: without it a resumed fold would read every persisted entry as recognised, and so prefer a
// curl label to the claude-code one the session saw next — the one ordering this change forbids.
func TestSummaryFold_UnrecognisedAgentSurvivesJSON(t *testing.T) {
	curl := pipeline.ParseUserAgent("curl/8.4.0")
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	want := usage.AgentLabel(curl)

	f := NewSummaryFold()
	f.Add("s1", &pipeline.SessionEvent{Client: curl})
	if got := f.Summary("s1").Agent; got != want {
		t.Fatalf("fold agent = %q, want %q", got, want)
	}

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resumed := NewSummaryFold()
	if err := json.Unmarshal(b, resumed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resumed.Summary("s1").Agent; got != want {
		t.Errorf("resumed agent = %q, want %q: the unknown flag did not survive session.json", got, want)
	}
	// A recognised agent arriving after the resume takes the row, exactly as it would have in
	// one uninterrupted fold.
	resumed.Add("s1", &pipeline.SessionEvent{Client: claude})
	if got := resumed.Summary("s1").Agent; got != "claude-code" {
		t.Errorf("resumed agent after claude-code = %q, want claude-code", got)
	}
}
