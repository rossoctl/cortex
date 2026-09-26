package sessionapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The claim writeSessionView makes: the same bytes json.Encoder produced.
//
// Worth a table rather than one case, because the difference between the two encoders is
// entirely in the envelope — the field order, the omitempty on totalEvents, nil vs empty
// events, the trailing newline — and every one of those is a place a hand-rolled writer
// can be plausibly wrong while looking right on a happy-path session.
func TestWriteSessionView_MatchesTheBufferedEncoding(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		view *pipeline.SessionView
	}{
		{"nil events encode as null, not []", &pipeline.SessionView{ID: "s1"}},
		{"empty events", &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{}}},
		{"one event", &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{
			{At: at, Direction: pipeline.Inbound, Phase: pipeline.SessionRequest},
		}}},
		{"several events", &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{
			{At: at, Phase: pipeline.SessionRequest},
			{At: at.Add(time.Second), Phase: pipeline.SessionResponse, StatusCode: 200},
			{At: at.Add(2 * time.Second), Phase: pipeline.SessionDenied, StatusCode: 401},
		}}},
		{"totalEvents present when the view is a tail", &pipeline.SessionView{
			ID: "s1", TotalEvents: 5071,
			Events: []pipeline.SessionEvent{{At: at}},
		}},
		// Both truncation fields together, and each alone. writeSessionView emits them by
		// hand, so a field added to SessionView and not to the writer is only caught if a
		// fixture actually sets it — these are what make that promise true.
		{"oldestSeq present when the view is a page", &pipeline.SessionView{
			ID: "s1", TotalEvents: 5071, OldestSeq: 2048,
			Events: []pipeline.SessionEvent{{At: at, Seq: 3000}},
		}},
		{"oldestSeq without totalEvents", &pipeline.SessionView{
			ID: "s1", OldestSeq: 7,
			Events: []pipeline.SessionEvent{{At: at, Seq: 9}},
		}},
		{"an id needing escapes", &pipeline.SessionView{
			ID:     `a/b "c" <d>&e` + "\né世",
			Events: []pipeline.SessionEvent{{At: at}},
		}},
		{"content needing HTML escapes", &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{{
			At: at,
			Inference: &pipeline.InferenceExtension{
				Model:      "m",
				Messages:   []pipeline.InferenceMessage{{Role: "user", Content: `<script>a && b</script>`}},
				Completion: "   \x00 ünïcødé 世界",
			},
		}}}},
		// Every field of sessionEventWire, so the name is not promising more than the
		// case covers. Both paths hand the event to the same SessionEvent.MarshalJSON, so
		// they cannot diverge per field — what this actually pins is that the ENVELOPE
		// stays right around an event large enough to span a buffer boundary.
		{"every wire field populated", &pipeline.SessionView{ID: "s1", TotalEvents: 3, Events: []pipeline.SessionEvent{{
			SessionID:  "s1",
			At:         at,
			Direction:  pipeline.Outbound,
			Phase:      pipeline.SessionResponse,
			RequestID:  "req-7",
			Host:       "api.example",
			HTTPMethod: "POST",
			HTTPPath:   "/v1/messages",
			StatusCode: 200,
			Duration:   12 * time.Millisecond,
			Identity:   &pipeline.EventIdentity{Subject: "alice", ClientID: "agent", Scopes: []string{"openid"}},
			Error:      &pipeline.EventError{Kind: "upstream", Code: "503", Message: "unavailable"},
			A2A:        &pipeline.A2AExtension{Method: "message/send", Parts: []pipeline.A2APart{{Content: "hi"}}, IsAction: true},
			MCP: &pipeline.MCPExtension{
				Method:   "tools/call",
				RPCID:    7,
				Params:   map[string]any{"name": "get_weather", "arguments": map[string]any{"city": "Zürich"}},
				Result:   map[string]any{"content": []any{"18°C"}},
				Err:      &pipeline.MCPError{Code: -32602, Message: "invalid params", Data: "city"},
				IsAction: true,
			},
			Inference: &pipeline.InferenceExtension{
				Model:     "claude",
				Messages:  []pipeline.InferenceMessage{{Role: "user", Content: "q"}},
				Tools:     []pipeline.InferenceTool{{Name: "t", Description: "d", Parameters: `{"type": "object"}`}},
				ToolCalls: []pipeline.InferenceToolCall{{ID: "1", Name: "t", Arguments: `{"a":1}`}},
			},
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{
				Plugin: "token-exchange", Action: pipeline.ActionModify, Reason: "exchanged",
				Details: map[string]string{"target_audience": "aud"},
			}}},
			Plugins:      map[string]json.RawMessage{"cost": json.RawMessage(`{"usd":0.0123}`)},
			TLS:          &pipeline.EventTLS{Version: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", PeerSPIFFEID: "spiffe://example/agent"},
			Tunnel:       true,
			TunnelReason: pipeline.TunnelClientRejectedCA,
		}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want bytes.Buffer
			if err := json.NewEncoder(&want).Encode(tc.view); err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := writeSessionView(&got, tc.view); err != nil {
				t.Fatal(err)
			}
			if got.String() != want.String() {
				t.Errorf("streamed output differs from json.Encoder\n got: %s\nwant: %s", got.String(), want.String())
			}
			// And it is still valid JSON that round-trips, so a mismatch the string
			// compare somehow tolerated cannot pass here either.
			var back pipeline.SessionView
			if err := json.Unmarshal(got.Bytes(), &back); err != nil {
				t.Fatalf("output does not parse: %v", err)
			}
			if back.ID != tc.view.ID || len(back.Events) != len(tc.view.Events) ||
				back.TotalEvents != tc.view.TotalEvents || back.OldestSeq != tc.view.OldestSeq {
				t.Errorf("round-trip lost fields: id=%q events=%d total=%d oldestSeq=%d",
					back.ID, len(back.Events), back.TotalEvents, back.OldestSeq)
			}
		})
	}
}

// The point of the change: the document reaches the writer in per-event pieces, so the
// peak buffer is one event and not the response.
//
// Asserted on write sizes rather than on heap, because that is deterministic. bufio hands
// through any write larger than its buffer, so the largest write this makes is one event's
// encoding — where json.Encoder.Encode writes the entire document in a single call, which
// is the allocation that put 244MB on the heap for a 105MB response. The buffered
// encoder's own largest write is measured here rather than assumed, so this fails if
// writeSessionView ever starts accumulating instead of streaming.
func TestWriteSessionView_NeverBuffersTheWholeDocument(t *testing.T) {
	const events = 40
	view := &pipeline.SessionView{ID: "s1"}
	// Fixed, not time.Now(): RFC3339Nano trims trailing zeros, so a wall-clock timestamp
	// makes the byte counts in this test's own failure message vary run to run — which is
	// no help to whoever is reading it to find out how far off the bound they are.
	at := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	for i := 0; i < events; i++ {
		view.Events = append(view.Events, pipeline.SessionEvent{
			At: at,
			Inference: &pipeline.InferenceExtension{
				Model: "m",
				Messages: []pipeline.InferenceMessage{{
					Role:    "user",
					Content: fmt.Sprintf("event %02d: %s", i, strings.Repeat("prompt text ", 6000)),
				}},
			},
		})
	}

	oneEvent, err := json.Marshal(&view.Events[0])
	if err != nil {
		t.Fatal(err)
	}

	var streamed, buffered maxWriter
	if err := writeSessionView(&streamed, view); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&buffered).Encode(view); err != nil {
		t.Fatal(err)
	}

	if streamed.total != buffered.total {
		t.Fatalf("streamed %d bytes, buffered %d — the two must emit the same document",
			streamed.total, buffered.total)
	}
	t.Logf("document %d bytes; largest single write: streamed %d, buffered %d",
		streamed.total, streamed.max, buffered.max)

	// The bound: one event plus whatever the buffer had already accumulated.
	if limit := len(oneEvent) + snapshotWriteBuffer; streamed.max > limit {
		t.Errorf("largest streamed write is %d bytes, over the one-event bound of %d — "+
			"the document is being accumulated, not streamed", streamed.max, limit)
	}
	// And the contrast is real, not an artifact of a document too small to tell apart.
	if buffered.max < streamed.total {
		t.Errorf("buffered encoder's largest write is %d of %d bytes; this test needs a "+
			"document the buffered path emits in one write to be measuring anything",
			buffered.max, streamed.total)
	}
}

// maxWriter records how much the writer was ever handed at once.
type maxWriter struct {
	max   int
	total int
}

func (m *maxWriter) Write(p []byte) (int, error) {
	if len(p) > m.max {
		m.max = len(p)
	}
	m.total += len(p)
	return len(p), nil
}

// A marshal failure part-way through is where streaming differs from Encode in behavior
// and not just in cost, so the difference is pinned rather than left to be discovered.
//
// The response is not atomic any more: bytes are already out under a 200, so the client
// gets a truncated document. What this asserts is the part that was a choice — the cut
// falls where the failure was, not at whatever buffer boundary preceded it, and the error
// names the event so the handler's log line locates it.
func TestWriteSessionView_OnMarshalFailureCutsAtTheFailureAndNamesIt(t *testing.T) {
	view := &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{
		{Host: "first.example"},
		{Host: "second.example"},
		// json.RawMessage is emitted verbatim and then validated, so invalid content here
		// is the one marshal error a SessionEvent can produce.
		{Host: "third.example", Plugins: map[string]json.RawMessage{"broken": json.RawMessage(`{not json`)}},
	}}

	var got bytes.Buffer
	err := writeSessionView(&got, view)
	if err == nil {
		t.Fatal("an unmarshalable event produced no error")
	}
	if want := "marshal event 2 of 3"; !strings.Contains(err.Error(), want) {
		t.Errorf("error does not name the failing event, want %q in: %v", want, err)
	}

	// The events already committed to the document reached the writer instead of dying in
	// the buffer, and the failing one did not.
	for _, host := range []string{"first.example", "second.example"} {
		if !strings.Contains(got.String(), host) {
			t.Errorf("event %q was committed to the document but discarded on the error path", host)
		}
	}
	if strings.Contains(got.String(), "third.example") {
		t.Error("the failing event was partly written")
	}
	// Truncated, and truncated in the way the doc comment says: the client sees invalid
	// JSON rather than a short-but-parseable document that looks complete.
	if json.Valid(got.Bytes()) {
		t.Error("a truncated response parsed as valid JSON, which would hide the failure")
	}
}

// A write error must surface, not be swallowed by the buffer.
func TestWriteSessionView_ReportsWriteErrors(t *testing.T) {
	view := &pipeline.SessionView{ID: "s1", Events: []pipeline.SessionEvent{
		{Inference: &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: strings.Repeat("x", 128<<10)},
		}}},
	}}
	if err := writeSessionView(failWriter{}, view); err == nil {
		t.Error("a failing writer produced no error")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("connection reset") }
