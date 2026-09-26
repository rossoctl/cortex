package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// responseRejectPlugin rejects on the response phase and counts how often it ran, which is the
// only thing this test is about. It declares no body access, so the listener does not defer to
// a body phase and handleResponseHeaders runs the response phase itself.
type responseRejectPlugin struct{ responses int }

func (p *responseRejectPlugin) Name() string { return "response-reject" }
func (p *responseRejectPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *responseRejectPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// An invocation, so the REQUEST phase records a session row and the response recorder has
	// a session to append to. Without it nothing is recorded at all and the row assertion
	// below would hold for the wrong reason.
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseRejectPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.responses++
	// An invocation before the denial, which is what a real policy plugin does — and what
	// makes the response recordable at all: recordOutboundResponseSession appends only when
	// the response carried an invocation, MCP, inference or plugin event.
	pctx.Observe("checked")
	return pipeline.Deny("policy.denied", "no")
}

// THE RESPONSE PHASE RUNS ONCE, EVEN WHEN IT REJECTS.
//
// The teardown flush has to finalize a response whose phase never ran — that is the
// headers-then-teardown fix — and it decided that by asking whether a BODY message had arrived.
// A rejected response answers that question the same way an unfinalized one does:
// handleResponseHeaders ran the phase and returned an ImmediateResponse, and rejectFromAction
// records nothing, so the flush ran the phase a second time. No double charge — both latches
// hold — but response-phase plugins executed twice, the recorded event gained a second reject
// invocation, and setRejectingPlugin re-fired immediately before RunFinish read the outcome.
//
// Two claims here, and the second is a decision rather than a bug: a rejected response is
// already finished, so the flush leaves it alone rather than recording a SessionResponse row
// for a denial. If such a row is ever wanted it needs a denied-phase recorder of its own.
func TestExtProc_AResponseRejectRunsTheResponsePhaseOnce(t *testing.T) {
	plug := &responseRejectPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{plug})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	empty, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New(nil): %v", err)
	}
	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		InboundPipeline:  pipeline.NewHolder(empty),
		Sessions:         store,
	}

	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":path", "/v1/models",
					":authority", "litellm.local",
				),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers:     makeHeaders(":status", "200", "content-type", "application/json"),
				EndOfStream: false,
			},
		}},
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed mid-script", stream.recvIdx, len(reqs))
	}

	if plug.responses != 1 {
		t.Errorf("OnResponse ran %d times, want 1: the teardown flush inferred that the phase "+
			"had not run from the absence of a body message, and a rejected response looks "+
			"identical to an unfinalized one under that inference", plug.responses)
	}
	// And no response row for a denial: the flush leaves a rejected response alone rather than
	// labelling it as an ordinary response.
	if ev := responseEvent(t, store); ev != nil {
		t.Errorf("a SessionResponse row was recorded for a REJECTED response (%+v); a denial "+
			"needs its own phase, not this one", ev)
	}
}
