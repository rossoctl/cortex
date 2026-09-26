package sseframe

import (
	"io"
	"strings"
	"testing"
)

// bom is U+FEFF as it arrives on the wire, written in escapes because a literal one in a Go source
// file is a compile error ("illegal byte order mark").
const bom = "\xef\xbb\xbf"

// anthropicTurn is the shape Anthropic and LiteLLM actually emit: an "event:" line naming the type,
// then the "data:" payload.
//
// THE event:-FIRST SHAPE IS THE ONE THAT MATTERS, and using a data:-only fixture measures a stream
// nothing sends. It is also what every other SSE fixture in this repo uses — reader_test.go here,
// forwardproxy's gzip and write-SSE tests, reverseproxy's streaming tests, litellm-budget-track's
// frames — so a fixture that drops the event lines is testing a shape no other test does.
const anthropicTurn = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"

// TestReadFrame_LeadingBOM_CostsTheFirstEventItsType is what a leading byte-order mark actually
// costs, measured on the wire shape gateways send.
//
// The spec says to remove one: "If the stream begins with a U+FEFF BYTE ORDER MARK character, then
// remove it." Nothing did, so the first LINE parses as a field whose name begins with those three
// bytes — matching neither "data" nor "event" — and is skipped.
//
// ON THE event:-FIRST SHAPE THAT SKIPS THE "event:" LINE, NOT THE EVENT. Measured against the reader
// before this fix:
//
//	data:-only, no BOM     2 frames, prompt tally present, LastEvent ""
//	data:-only + BOM       1 frame,  prompt tally LOST,     LastEvent ""
//	event:-first, no BOM   2 frames, prompt tally present, LastEvent "message_start"
//	event:-first + BOM     2 frames, prompt tally present, LastEvent ""
//
// So the frame and its payload survive on real traffic, and what goes missing is the first frame's
// TYPE. That is not cosmetic: a re-framing proxy reproduces the upstream "event:" line from
// LastEvent (see the field's doc, and forwardproxy's write-SSE test), and clients like the Anthropic
// SDK type each event from that field rather than from the payload. Losing it relays event #1
// untyped, which parses to zero typed events downstream.
//
// The data:-only rows stay because that shape is legal SSE and the loss there IS the whole event —
// but nothing in this repo emits it, so it is the lesser claim rather than the headline.
func TestReadFrame_LeadingBOM_CostsTheFirstEventItsType(t *testing.T) {
	const dataOnly = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"

	for _, tc := range []struct {
		name string
		in   string
		// wantEvents is how many events the reader surfaces, wantFirst what the first one's payload
		// must contain, and wantFirstType the event type it must carry. Every row names all three,
		// so no assertion is gated on another row's shape.
		wantEvents    int
		wantFirst     string
		wantFirstType string
	}{{
		name: "event-first, no BOM",
		in:   anthropicTurn, wantEvents: 2, wantFirst: "input_tokens", wantFirstType: "message_start",
	}, {
		// THE ROW THIS FIX IS FOR. Same events, same payloads, and before the fix the type was gone.
		name: "event-first with a leading BOM",
		in:   bom + anthropicTurn, wantEvents: 2, wantFirst: "input_tokens", wantFirstType: "message_start",
	}, {
		name: "data-only, no BOM",
		in:   dataOnly, wantEvents: 2, wantFirst: "input_tokens", wantFirstType: "",
	}, {
		// On this shape the BOM took the whole event, because there is no "event:" line in front of
		// it to absorb the damage.
		name: "data-only with a leading BOM",
		in:   bom + dataOnly, wantEvents: 2, wantFirst: "input_tokens", wantFirstType: "",
	}, {
		// TWO marks is one at the start plus a stray at the head of the first line — measured, not
		// assumed: my first guess for this row was wrong. On the event:-first shape the residue
		// lands on the "event:" line, so both events survive and only the TYPE is lost, which is
		// exactly the single-BOM damage the fix removes. Every row names its payload and its type
		// for this reason: a count alone stays green when the wrong event survives.
		name: "a doubled BOM keeps only the spec's rule",
		in:   bom + bom + anthropicTurn, wantEvents: 2, wantFirst: "input_tokens", wantFirstType: "",
	}, {
		// The same residue on the data:-only shape takes the whole first event, since there is no
		// "event:" line in front of the payload to absorb it. Both doubled rows are here because
		// together they say what one mark being removed means: the second one lands wherever the
		// first line happens to start.
		name: "a doubled BOM on a data-only stream loses the event",
		in:   bom + bom + dataOnly, wantEvents: 1, wantFirst: "output_tokens", wantFirstType: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			events, types := readAll(t, tc.in)
			if len(events) != tc.wantEvents {
				t.Fatalf("got %d events, want %d: %q", len(events), tc.wantEvents, events)
			}
			if !strings.Contains(events[0], tc.wantFirst) {
				t.Errorf("first event = %q, want one containing %q: the wrong event survived", events[0], tc.wantFirst)
			}
			if types[0] != tc.wantFirstType {
				t.Errorf("first event's type = %q, want %q — a re-framing proxy reproduces the upstream \"event:\" line from this, and a client that types events from it sees an untyped one",
					types[0], tc.wantFirstType)
			}
		})
	}
}

// TestReadFrame_BOMIsNotStrippedMidStream pins the other half of "once, at the start".
//
// The flag lives on the Reader so the check cannot re-run per frame. Scanning further would mean
// deleting those three bytes from any payload that legitimately contains them — a real risk here,
// since these frames carry arbitrary JSON, including whatever text a model generated.
//
// The mark sits at a LINE START, which is the only position that discriminates: inside a payload the
// check never looks there, so a fixture built that way passes whether the flag exists or not.
func TestReadFrame_BOMIsNotStrippedMidStream(t *testing.T) {
	events, _ := readAll(t, "data: first\n\n"+bom+"data: second\n\n")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — the second event's field name carries the BOM, so it is not \"data\" and the event is skipped: %q", len(events), events)
	}
	if events[0] != "first" {
		t.Errorf("first event = %q, want %q", events[0], "first")
	}
}

// readAll returns each event's payload and its event type, so a caller can assert both.
func readAll(t *testing.T, in string) (payloads, types []string) {
	t.Helper()
	r := NewReader(strings.NewReader(in), 1<<20)
	for {
		frame, err := r.ReadFrame()
		if err == io.EOF {
			return payloads, types
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		payloads = append(payloads, string(frame))
		// Read BEFORE the next ReadFrame, which reuses the buffer.
		types = append(types, string(r.LastEvent()))
	}
}
