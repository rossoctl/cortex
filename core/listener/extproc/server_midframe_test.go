package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/costevent"
)

// midFrameSplitRequests scripts the same streamed turn as splitStreamRequests, split at a byte
// offset INSIDE an event rather than on a frame boundary.
//
// Envoy's ResponseBody messages are chunks of a byte stream, not events: nothing aligns them
// with "\n\n", and for a body arriving over TCP nothing could. So the interesting split is the
// one that lands in the middle of the event carrying the output tally — every fixture in this
// package until now split between events, which is the one case that needs no reassembly.
func midFrameSplitRequests() []*extprocv3.ProcessingRequest {
	reqs := splitStreamRequests()
	// Keep the request phases and the response headers; replace the two body messages.
	head := reqs[:3]
	const first = "data: {\"type\":\"message_start\",\"message\":" +
		"{\"usage\":{\"input_tokens\":1000}}}\n\n" +
		// The delta event begins here and is CUT MID-FIELD: "outp" is half of
		// "output_tokens", so neither piece is a parseable event on its own.
		"data: {\"type\":\"message_delta\",\"delta\":" +
		"{\"stop_reason\":\"end_turn\"},\"usage\":{\"outp"
	const second = "ut_tokens\":500}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	return append(head,
		&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{Body: []byte(first), EndOfStream: false},
		}},
		&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseBody{
			ResponseBody: &extprocv3.HttpBody{Body: []byte(second), EndOfStream: true},
		}},
	)
}

// TestExtProc_SSEBodySplitMidFrame_ChargesTheWholeFigure is suggestion 6 of review round 6.
//
// THE SAME UNDERCOUNT THE NON-SSE ARM WAS JUST FIXED FOR, on the arm that was left alone. The
// SSE arm replaced pctx.ResponseBody per message and built a fresh sseframe.Reader over it, so
// an event split across two messages was parsed as two broken ones: sseframe delivers the
// unterminated tail of the first message as a frame (per spec, a stream may end without a
// blank line), and the remainder in the second message reads as a field named "ut_tokens" that
// nothing consumes. Both halves are discarded, and the tally they carried with them.
//
// WHICH TALLY DECIDES THE SIZE OF THE ERROR. Anthropic puts the output count on message_delta,
// so the half that goes missing is the OUTPUT half — 10x burndown on Bedrock, and the
// expensive part of any generation. The response still settles, at the prompt-only floor, which
// is why nothing about it looks broken.
func TestExtProc_SSEBodySplitMidFrame_ChargesTheWholeFigure(t *testing.T) {
	srv, store := newStreamedServer(t)
	reqs := midFrameSplitRequests()

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a mid-frame split body")
	}
	rec, ok := costevent.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row (Plugins keys = %v)", pluginKeys(ev))
	}
	switch rec.CostUSD {
	case streamedWholeUSD:
	case streamedFloorUSD:
		t.Errorf("CostUSD = %v, the prompt-only floor, want the whole figure %v: the event carrying the output tally was split across two ResponseBody messages and neither half parsed",
			rec.CostUSD, streamedWholeUSD)
	default:
		t.Errorf("CostUSD = %v, want the whole figure %v", rec.CostUSD, streamedWholeUSD)
	}
	// AND NOT DISCLOSED AS A FLOOR EITHER, which is the trap in fixing this by disclosure
	// instead: Incomplete on a stream that reported everything would send an operator hunting a
	// truncation that never happened.
	if rec.Incomplete {
		t.Errorf("Incomplete = true with reason %q: every count this turn reported did arrive, just not in one message", rec.IncompleteReason)
	}
}

// TestLastSSEFrameBoundary covers the three terminators SSE allows, because this is the one
// piece of framing logic in this package that is hand-rolled rather than delegated to sseframe
// — and it has to agree with sseframe.readLine or the two disagree about where an event ended.
//
// A wrong answer here is silent in both directions: too early leaves complete events in the
// carry, where the next message re-parses them and DOUBLES their counts; too late cuts an
// unfinished event loose, which is the undercount this whole fix is about.
func TestLastSSEFrameBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"one complete event", "data: a\n\n", 9},
		{"a complete event and a fragment", "data: a\n\ndata: b", 9},
		{"no boundary at all", "data: a", 0},
		{"CRLF", "data: a\r\n\r\n", 11},
		{"bare CR", "data: a\r\r", 9},
		{"two events, cut in the third", "data: a\n\ndata: b\n\ndata: c", 18},
		{"a comment line does not end an event", ": ping\ndata: a", 0},
		// A heartbeat comment IS followed by a blank line in the wild, and that blank line
		// terminates an event with no data — sseframe skips the empty event, and this scan
		// still reports the offset, which is what makes the carry empty rather than repeated.
		{"a terminated comment", ": ping\n\n", 8},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastSSEFrameBoundary([]byte(tc.in)); got != tc.want {
				t.Errorf("lastSSEFrameBoundary(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
