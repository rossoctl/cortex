package extproc

import (
	"context"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// responseCounter counts response-phase dispatches. Deliberately NOT a StreamingResponder: it
// implements no OnResponseFrame, so RunResponse is the only hook that reaches it — which is
// exactly the population the teardown flush can double.
type responseCounter struct {
	mu    sync.Mutex
	calls int
}

func (p *responseCounter) Name() string { return "response-counter" }
func (p *responseCounter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *responseCounter) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseCounter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	// An Observe per pass, so a doubled phase is visible in the recorded row too and not only
	// in this counter.
	pctx.Observe("response phase")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseCounter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestExtProc_TornStreamRunsTheResponsePhaseOnce is suggestion 4 of review round 6.
//
// THE FLUSH MUST NOT RUN A PHASE THAT ALREADY RAN. On the route where body messages arrive but
// Envoy never says end-of-stream, the response phase and the teardown flush could each dispatch it,
// and every non-streaming plugin then decided twice — opa, cpex, lineage, sparc in the shipped
// pipelines — with the extra Invocation rows landing in the recorded snapshot.
//
// What makes it once is that a dispatch site RECORDS the response and the flush skips a recorded
// one. An explicit "phase ran" mark existed for a while and was unreachable: deleting both of its
// call sites left this package green, because no path marks without recording.
//
// MONEY WAS NEVER AT RISK HERE, and saying so is part of the claim: the cost owner is a
// StreamingResponder, and RunResponse skips those, so the settle latch was never the thing
// standing between this and a double charge. What was at risk is the audit trail — a request
// whose policy plugins are recorded as having decided twice.
func TestExtProc_TornStreamRunsTheResponsePhaseOnce(t *testing.T) {
	counter := &responseCounter{}
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(streamedRates(t))
	// The parser is present because it is what makes the pipeline need a body at all: without
	// it handleResponseHeaders would not defer the phase, and this route would not exist.
	p, err := pipeline.New([]pipeline.Plugin{parser, counter})
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := tornStreamRequests()
	stream := &cancelOnEndStream{mockStream: &mockStream{ctx: ctx, requests: reqs}, cancel: cancel}

	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed before the teardown", stream.recvIdx, len(reqs))
	}
	if ctx.Err() == nil {
		t.Fatal("the stream context is not cancelled, so this test is not exercising teardown")
	}

	if got := counter.count(); got != 1 {
		t.Errorf("response phases = %d, want 1: the teardown flush ran every non-streaming plugin a second time", got)
	}
}

// TestExtProc_MultiMessageBodyRunsTheResponsePhaseOnce is item 6 of review round 7.
//
// The phase is a per-RESPONSE dispatch and was written as a per-MESSAGE one. A statically
// configured STREAMED body mode delivers N messages, and each ran RunResponse over a PARTIAL
// body: N decisions on N prefixes of a document, all of their Invocation rows in the recorded
// snapshot. It is also the same "wait for the whole body" rule the frame dispatch and the session record already follow.
//
// No double charge was reachable — both cost owners are StreamingResponders, which RunResponse
// skips — so what this protects is the audit trail, and any plugin that reads pctx.ResponseBody
// expecting a whole document.
func TestExtProc_MultiMessageBodyRunsTheResponsePhaseOnce(t *testing.T) {
	counter := &responseCounter{}
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(streamedRates(t))
	p, err := pipeline.New([]pipeline.Plugin{parser, counter})
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

	// The shipped split fixture: two body messages, end-of-stream on the second.
	reqs := splitStreamRequests()
	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	if got := counter.count(); got != 1 {
		t.Errorf("response phases = %d over a two-message body, want 1: each extra one decided over a partial body and recorded an Invocation row for it", got)
	}
}

// lateRejecter refuses the response phase. In the flush it is a refusal that arrives after Envoy
// has already delivered the response — a decision that cannot take effect.
type lateRejecter struct{}

func (lateRejecter) Name() string { return "late-rejecter" }
func (lateRejecter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (lateRejecter) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (lateRejecter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// DenyAndRecord, because that is what a real gate does: the Invocation is the audit trail and
	// the Action is the decision. Returning a bare Deny would leave only the framework's
	// rejecting-plugin state, and this test is about both.
	return pctx.DenyAndRecord("late.reject", "late.reject", "refused after the response was delivered")
}

// finishWatcher records the outcome the listener HANDED to RunFinish, which is what
// pctx.Outcome() returns to a plugin during its finish pass.
type finishWatcher struct {
	outcomes  []pipeline.Outcome
	derived   []pipeline.Outcome
	rejecting []string
}

func (f *finishWatcher) Name() string { return "finish-watcher" }
func (f *finishWatcher) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (f *finishWatcher) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (f *finishWatcher) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (f *finishWatcher) OnFinish(_ context.Context, pctx *pipeline.Context) {
	f.rejecting = append(f.rejecting, pctx.RejectingPlugin())
	if out := pctx.Outcome(); out != nil {
		f.outcomes = append(f.outcomes, *out)
	}
	// AND THE ONE A PLUGIN DERIVES ITSELF, which is the harder claim: passing a corrected
	// outcome to RunFinish fixes only what the listener asserts, while any plugin is free to ask
	// the context directly. Both are recorded so the test can tell them apart.
	f.derived = append(f.derived, pipeline.OutcomeFromContext(pctx))
}

// TestExtProc_ALateRejectDoesNotInvertTheOutcome is item 8 of review round 7.
//
// The flush's RunResponse dropped its Action but not its side effects. A plugin rejecting there
// records the refusal on the context, and pipeline.OutcomeFromContext maps any deny to
// OutcomeDeny — so deriving the outcome AFTER the flush told every Finisher that a request Envoy
// answered with a 200 had been denied. That inverts the rule this same defer states for a
// response rejected during normal processing, where the early return exists precisely so a
// denial and an ordinary response are not confused.
//
// Measured before the fix: FinalAction=deny, DenyingPlugin=late-rejecter, StatusCode=200 — the
// two halves of one outcome contradicting each other. The recorded row was never wrong, which is
// why this needed a Finisher to see: the damage is in what plugins are told, not in the event.
//
// THE ONLY ROUTE THAT REACHES IT is response headers with no body message at all and then a
// teardown, because that is the one case where the flush runs the response phase itself.
func TestExtProc_ALateRejectDoesNotInvertTheOutcome(t *testing.T) {
	watcher := &finishWatcher{}
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(streamedRates(t))
	p, err := pipeline.New([]pipeline.Plugin{watcher, parser, lateRejecter{}})
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

	// Response headers, then nothing: the body message the header phase deferred to never
	// arrives, and the stream is torn down.
	reqs := tornStreamRequests()
	for i := range reqs {
		if reqs[i].GetResponseBody() != nil {
			reqs = reqs[:i]
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &cancelOnEndStream{mockStream: &mockStream{ctx: ctx, requests: reqs}, cancel: cancel}
	_ = srv.Process(stream)

	if len(watcher.outcomes) != 1 {
		t.Fatalf("finish outcomes = %d, want exactly 1: %+v", len(watcher.outcomes), watcher.outcomes)
	}
	got := watcher.outcomes[0]
	if got.FinalAction == pipeline.OutcomeDeny {
		t.Errorf("outcome = %+v: a refusal that arrived after Envoy delivered the response cannot make the request a denial, and this row's own StatusCode says it was answered",
			got)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	if len(watcher.derived) != 1 {
		t.Fatalf("derived outcomes = %d, want exactly 1: %+v", len(watcher.derived), watcher.derived)
	}
	if d := watcher.derived[0]; d.FinalAction == pipeline.OutcomeDeny {
		t.Errorf("a plugin deriving the outcome from the context sees %+v: the listener passing a corrected outcome to RunFinish is not enough, because nothing stops a plugin asking the context — see Context.MarkResponseDelivered",
			d)
	}
	// AND THE REFUSAL IS STILL ON THE RECORD, marked rather than erased: a plugin that would
	// have refused a response after it shipped is exactly what a rollout wants to see.
	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no response row recorded for a delivered response")
	}
	var lateDeny bool
	if ev.Invocations != nil {
		for _, inv := range ev.Invocations.Outbound {
			if inv.Action == pipeline.ActionDeny && inv.Late {
				lateDeny = true
			}
		}
	}
	// THE EVIDENCE IS NOT ERASED EITHER. RejectingPlugin still names the plugin: what changed is
	// that the OUTCOME no longer reads that as a denial. Erasing it would hide a real event.
	if len(watcher.rejecting) != 1 || watcher.rejecting[0] != "late-rejecter" {
		t.Errorf("RejectingPlugin during finish = %v, want [late-rejecter]: the refusal is marked as too late, not forgotten", watcher.rejecting)
	}
	if !lateDeny {
		t.Errorf("no late deny invocation on the recorded row (%+v): the fix has to MARK the refusal, not drop it — the audit trail is the half worth keeping",
			ev.Invocations)
	}

}
