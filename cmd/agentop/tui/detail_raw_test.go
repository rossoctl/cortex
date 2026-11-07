package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"

	"github.com/rossoctl/cortex/core/pipeline"
)

// rawRequest is a captured Anthropic request with a thinking block, a tool call, and an image in
// a tool result: the parts the parsed event drops.
const rawRequest = `{"model":"m","messages":[` +
	`{"role":"user","content":"read it"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"the file is small, so read it whole","signature":"c2ln"},` +
	`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.png"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
	`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB" + `"}}]}]}]}`

// rawDetail opens the detail of a request event with its capture installed, as the fetch leaves it.
func rawDetail(t *testing.T, raw *pipeline.RawCapture) string {
	t.Helper()
	ev := &pipeline.SessionEvent{Seq: 3, Phase: pipeline.SessionRequest, Direction: pipeline.Outbound, Host: "h",
		Inference: &pipeline.InferenceExtension{Model: "m"}}
	m := detailModel(t, "s1", ev)
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 3, event: ev, raw: raw})
	// Every fold open, as a reader opening each in turn would leave it.
	m.detailFolds = map[string]foldChoice{}
	var openAll func(n *node)
	openAll = func(n *node) {
		m.detailFolds[n.path] = foldChoice{open: true}
		for _, k := range n.kids {
			openAll(k)
		}
	}
	openAll(m.detailTree)
	m.renderDetail()
	return stripANSI(strings.Join(lineTexts(m.detailLines), "\n"))
}

func lineTexts(ls []treeLine) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.text
	}
	return out
}

// The exact request shows what the parsed event cannot: thinking where the model wrote it, and
// an image as its type and size rather than its base64.
func TestDetailRaw_ShowsThinkingInPlaceAndImagesBySize(t *testing.T) {
	out := rawDetail(t, &pipeline.RawCapture{Request: rawRequest,
		RequestHeaders: map[string][]string{"Anthropic-Beta": {"interleaved-thinking"}}})
	for _, want := range []string{"the file is small, so read it whole", "[image image/png 24B]", "interleaved-thinking"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "iVBORw0KGgo") {
		t.Errorf("the image's base64 is on screen:\n%s", out)
	}
}

// What a plugin changed is the first thing to say about a rewritten request.
func TestDetailRaw_SaysWhetherAPluginRewroteTheBody(t *testing.T) {
	fwd := `{"model":"n"}`
	if out := rawDetail(t, &pipeline.RawCapture{Request: `{"model":"m"}`, Forwarded: &fwd}); !strings.Contains(out, "rewritten by a plugin: 13B → 13B") {
		t.Errorf("no rewrite line:\n%s", out)
	}
	if out := rawDetail(t, &pipeline.RawCapture{Request: `{"model":"m"}`}); !strings.Contains(out, "forwarded as sent") {
		t.Errorf("no as-sent line:\n%s", out)
	}
}

// The raw section starts closed: it is large and seldom the point, like the tool manifest.
func TestDetailRaw_StartsClosedAndOpensWithEnter(t *testing.T) {
	ev := &pipeline.SessionEvent{Seq: 3, Phase: pipeline.SessionRequest, Direction: pipeline.Outbound, Host: "h",
		Inference: &pipeline.InferenceExtension{Model: "m"}}
	m := detailModel(t, "s1", ev)
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 3, event: ev, raw: &pipeline.RawCapture{Request: rawRequest}})
	if out := stripANSI(strings.Join(lineTexts(m.detailLines), "\n")); strings.Contains(out, "read it whole") {
		t.Fatalf("raw starts open:\n%s", out)
	}
	cursorTo(t, m, "raw")
	press(m, tea.KeyEnter)
	if out := stripANSI(strings.Join(lineTexts(m.detailLines), "\n")); !strings.Contains(out, "messages") {
		t.Fatalf("Enter did not open raw:\n%s", out)
	}
}

// A response's capture shows its status and flags, and a compressed body as its size.
func TestDetailRaw_Response(t *testing.T) {
	ev := &pipeline.SessionEvent{Seq: 4, Phase: pipeline.SessionResponse, Direction: pipeline.Outbound, Host: "h",
		Inference: &pipeline.InferenceExtension{Model: "m"}}
	m := detailModel(t, "s1", ev)
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 4, event: ev, raw: &pipeline.RawCapture{
		Response: &pipeline.RawBody{Status: 200, Headers: map[string][]string{"Content-Encoding": {"gzip"}},
			BodyBase64: make([]byte, 2048), Incomplete: true}}})
	m.detailFolds = map[string]foldChoice{"response.raw": {open: true}}
	m.renderDetail()
	out := stripANSI(strings.Join(lineTexts(m.detailLines), "\n"))
	for _, want := range []string{"200", "incomplete", "[gzip body 2.0kB]"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
}

// Enter fetches against a proxy that projects, the only kind with the per-event route, once per
// row: the capture comes with the fetch and is kept. A proxy that does not project has no such
// route, so asking would only flash a 404. A row with no body has nothing to fetch.
func TestDetailRaw_EnterFetches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ev       pipeline.SessionEvent
		projects bool
		fetched  bool
		want     bool
	}{
		{"inference row", turnEvent(), true, false, true},
		{"already fetched once", turnEvent(), true, true, false},
		{"older proxy", turnEvent(), false, false, false},
		{"tunnel row", pipeline.SessionEvent{Seq: 8, Phase: pipeline.SessionRequest, Host: "h:443", Tunnel: true}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newSearchEventsModel(t, "h")
			m.client, m.ctx = apiclient.New("http://127.0.0.1:1"), context.Background()
			m.serverProjects = tc.projects
			m.events["s"] = []pipeline.SessionEvent{tc.ev}
			if tc.fetched {
				m.markFullFetched("s", tc.ev.Seq)
			}
			m.rebuildEventsTable()
			setCursorVisible(&m.eventsTbl, 0)
			m.selectedEventKey = keyOf(m.selectedEvent())
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if m.pane != paneDetail {
				t.Fatalf("Enter left pane %v, want detail", m.pane)
			}
			if got := fetches(cmd); got != tc.want {
				t.Fatalf("Enter fetched the event: %v, want %v", got, tc.want)
			}
		})
	}
}

// A search does not open the raw section: it holds a second copy of the conversation and up to
// 10 MiB of response, so nearly every query matches in it, and drawing it made each keystroke
// take over a second. It opens when the reader opens it.
func TestDetailRaw_SearchLeavesRawClosed(t *testing.T) {
	m := openTurnDetail(t)
	m.detailRow.raw = &pipeline.RawCapture{Request: `{"messages":[{"role":"user","content":"only-in-raw needle"}]}`}
	m.showDetail(m.detailRow, false)
	typeKeys(m, "/needle")
	press(m, tea.KeyEnter)
	if out := detailScreen(m); strings.Contains(out, "only-in-raw") {
		t.Fatalf("the search opened raw:\n%s", out)
	}
}

// fetches runs cmd, and any batch it returns, and reports whether one of them is the detail fetch.
func fetches(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case detailEventLoadedMsg:
		return true
	case tea.BatchMsg:
		for _, c := range msg {
			if fetches(c) {
				return true
			}
		}
	}
	return false
}

// A row opened again shows the capture its first open fetched, without asking again.
func TestDetailRaw_ReopenShowsTheHeldCapture(t *testing.T) {
	m := newSearchEventsModel(t, "h")
	m.serverProjects = true
	m.events["s"] = []pipeline.SessionEvent{turnEvent()}
	m.markFullFetched("s", 7, &pipeline.RawCapture{Request: `{"model":"held"}`})
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	if m.detailRow.raw == nil || m.detailRow.raw.Request != `{"model":"held"}` {
		t.Fatalf("the reopened row shows raw %+v, want the held capture", m.detailRow.raw)
	}
}
