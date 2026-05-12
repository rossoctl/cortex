package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// finisherPlugin embeds stubPlugin and adds OnFinish. Using embedding
// rather than a brand-new struct so the tests reuse the existing
// Name/Capabilities/OnRequest/OnResponse surface.
type finisherPlugin struct {
	*stubPlugin
	onFinish func(ctx context.Context, pctx *Context)
}

func (f *finisherPlugin) OnFinish(ctx context.Context, pctx *Context) {
	if f.onFinish != nil {
		f.onFinish(ctx, pctx)
	}
}

func newFinisher(name string, onFinish func(ctx context.Context, pctx *Context)) *finisherPlugin {
	return &finisherPlugin{
		stubPlugin: &stubPlugin{name: name},
		onFinish:   onFinish,
	}
}

// TestRunFinish_NilWhenNoDispatch verifies that a pctx with an empty
// dispatched list (no Run was ever called, or the pipeline was empty)
// is a safe no-op. Prevents an accidental double-free on the edge case
// where a listener defers RunFinish but Run never reached dispatch.
func TestRunFinish_NilWhenNoDispatch(t *testing.T) {
	p, err := New([]Plugin{newFinisher("f", nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	// No panic, no OnFinish invocation (nothing in dispatched).
}

// TestRunFinish_LIFO verifies dispatch order is reverse of OnRequest
// order, symmetric with Shutdowner and RunResponse.
func TestRunFinish_LIFO(t *testing.T) {
	var order []string
	mk := func(name string) *finisherPlugin {
		return newFinisher(name, func(_ context.Context, _ *Context) {
			order = append(order, name)
		})
	}
	p, err := New([]Plugin{mk("a"), mk("b"), mk("c")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	if act := p.Run(context.Background(), pctx); act.Type != Continue {
		t.Fatalf("Run action = %v, want Continue", act.Type)
	}
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if got, want := order, []string{"c", "b", "a"}; !equal(got, want) {
		t.Errorf("finish order = %v, want %v (LIFO)", got, want)
	}
}

// TestRunFinish_OnlyDispatched verifies that OnFinish is NOT called on
// plugins whose OnRequest never ran because an earlier plugin denied.
// Plugin A reserves, B denies, C+D never dispatch. Only A and B (the
// denier) should see OnFinish; C and D must not.
func TestRunFinish_OnlyDispatched(t *testing.T) {
	var seen []string
	markSeen := func(name string) *finisherPlugin {
		return newFinisher(name, func(_ context.Context, _ *Context) {
			seen = append(seen, name)
		})
	}
	a := markSeen("a")
	b := markSeen("b")
	b.stubPlugin.onReq = func(ctx context.Context, pctx *Context) Action {
		return Action{Type: Reject, Violation: &Violation{Code: "test.deny", Reason: "b denied"}}
	}
	c := markSeen("c")
	d := markSeen("d")
	p, err := New([]Plugin{a, b, c, d})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	action := p.Run(context.Background(), pctx)
	if action.Type != Reject {
		t.Fatalf("Run action = %v, want Reject", action.Type)
	}
	p.RunFinish(context.Background(), pctx, Outcome{
		FinalAction:   OutcomeDeny,
		DenyingPlugin: "b",
	})
	// LIFO over [a, b]: b then a. c, d absent.
	if got, want := seen, []string{"b", "a"}; !equal(got, want) {
		t.Errorf("finish plugins = %v, want %v (only dispatched, LIFO)", got, want)
	}
}

// TestRunFinish_OutcomeVisible verifies pctx.Outcome() returns the
// supplied outcome during OnFinish.
func TestRunFinish_OutcomeVisible(t *testing.T) {
	var gotAction OutcomeAction
	var gotStatus int
	var gotDenier string
	var gotDurNonZero bool
	f := newFinisher("f", func(_ context.Context, pctx *Context) {
		o := pctx.Outcome()
		if o == nil {
			t.Fatal("pctx.Outcome() nil during OnFinish")
		}
		gotAction = o.FinalAction
		gotStatus = o.StatusCode
		gotDenier = o.DenyingPlugin
		gotDurNonZero = o.Duration > 0
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{StartedAt: time.Now().Add(-10 * time.Millisecond)}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{
		FinalAction:   OutcomeDeny,
		StatusCode:    503,
		DenyingPlugin: "upstream-gate",
	})
	if gotAction != OutcomeDeny {
		t.Errorf("FinalAction = %q, want deny", gotAction)
	}
	if gotStatus != 503 {
		t.Errorf("StatusCode = %d, want 503", gotStatus)
	}
	if gotDenier != "upstream-gate" {
		t.Errorf("DenyingPlugin = %q, want upstream-gate", gotDenier)
	}
	if !gotDurNonZero {
		t.Error("Duration should have been auto-derived from StartedAt and non-zero")
	}
}

// TestRunFinish_OutcomeNilOutsideFinish verifies the "nil everywhere
// except OnFinish" contract for pctx.Outcome().
func TestRunFinish_OutcomeNilOutsideFinish(t *testing.T) {
	var seenInRequest *Outcome
	var seenInResponse *Outcome
	var seenInFinish *Outcome
	f := newFinisher("f", func(_ context.Context, pctx *Context) {
		seenInFinish = pctx.Outcome()
	})
	f.stubPlugin.onReq = func(_ context.Context, pctx *Context) Action {
		seenInRequest = pctx.Outcome()
		return Action{Type: Continue}
	}
	f.stubPlugin.onResp = func(_ context.Context, pctx *Context) Action {
		seenInResponse = pctx.Outcome()
		return Action{Type: Continue}
	}
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunResponse(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if seenInRequest != nil {
		t.Errorf("pctx.Outcome() in OnRequest = %v, want nil", seenInRequest)
	}
	if seenInResponse != nil {
		t.Errorf("pctx.Outcome() in OnResponse = %v, want nil", seenInResponse)
	}
	if seenInFinish == nil {
		t.Error("pctx.Outcome() in OnFinish = nil, want non-nil")
	}
	// And cleared after RunFinish returns:
	if pctx.Outcome() != nil {
		t.Error("pctx.Outcome() after RunFinish returned != nil")
	}
}

// TestRunFinish_PanicRecovered verifies a panicking OnFinish doesn't
// prevent later plugins in the LIFO chain from running.
func TestRunFinish_PanicRecovered(t *testing.T) {
	var aRan, cRan atomic.Bool
	a := newFinisher("a", func(_ context.Context, _ *Context) {
		aRan.Store(true)
	})
	b := newFinisher("b", func(_ context.Context, _ *Context) {
		panic("b is a bug")
	})
	c := newFinisher("c", func(_ context.Context, _ *Context) {
		cRan.Store(true)
	})
	p, err := New([]Plugin{a, b, c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if !cRan.Load() {
		t.Error("plugin c (first in LIFO) should have run, didn't")
	}
	if !aRan.Load() {
		t.Error("plugin a (after panicking b) should have run, didn't")
	}
}

// TestRunFinish_FreshContextNotCancelled verifies OnFinish receives a
// ctx that is NOT derived from a cancelled request ctx. The request
// ctx is cancelled (simulating client disconnect); the framework must
// still dispatch OnFinish with a live ctx so plugin I/O doesn't fail
// immediately.
func TestRunFinish_FreshContextNotCancelled(t *testing.T) {
	var ctxErrAtFinish error
	f := newFinisher("f", func(ctx context.Context, _ *Context) {
		ctxErrAtFinish = ctx.Err()
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}

	// Simulate client disconnect by pre-cancelling the request ctx.
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// Run Run with a fresh ctx so the pipeline doesn't short-circuit
	// on ctx.Err() before even dispatching — that's a separate path.
	p.Run(context.Background(), pctx)

	// But RunFinish is called with the (cancelled) request ctx. The
	// fresh-ctx contract means OnFinish's ctx is NOT this cancelled one.
	p.RunFinish(reqCtx, pctx, Outcome{FinalAction: OutcomeAllow})

	if ctxErrAtFinish != nil {
		t.Errorf("OnFinish ctx.Err() = %v, want nil (framework must supply a fresh ctx)", ctxErrAtFinish)
	}
}

// TestRunFinish_FreshContextHasDeadline verifies the fresh ctx carries
// the framework-set finish timeout so a hung plugin is eventually
// unblocked.
func TestRunFinish_FreshContextHasDeadline(t *testing.T) {
	var hadDeadline bool
	var deadlineRemaining time.Duration
	f := newFinisher("f", func(ctx context.Context, _ *Context) {
		d, ok := ctx.Deadline()
		hadDeadline = ok
		if ok {
			deadlineRemaining = time.Until(d)
		}
	})
	// Pick an override small enough to see but large enough to reliably
	// be positive-remaining when OnFinish reads it.
	p, err := New([]Plugin{f}, WithFinishTimeout(250*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if !hadDeadline {
		t.Fatal("OnFinish ctx had no deadline; expected framework-set timeout")
	}
	if deadlineRemaining <= 0 || deadlineRemaining > 250*time.Millisecond {
		t.Errorf("deadline remaining = %v, want in (0, 250ms]", deadlineRemaining)
	}
}

// TestRunFinish_DefaultTimeout verifies WithFinishTimeout defaults to
// DefaultFinishTimeout when not supplied or when supplied as zero.
func TestRunFinish_DefaultTimeout(t *testing.T) {
	p1, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p1.finishTimeout != DefaultFinishTimeout {
		t.Errorf("unset finishTimeout = %v, want default %v", p1.finishTimeout, DefaultFinishTimeout)
	}
	p2, err := New(nil, WithFinishTimeout(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p2.finishTimeout != DefaultFinishTimeout {
		t.Errorf("explicit 0 finishTimeout = %v, want default %v", p2.finishTimeout, DefaultFinishTimeout)
	}
}

// TestRunFinish_OffPolicyPluginNotDispatched verifies a plugin wired
// with ErrorPolicyOff is not invoked by OnRequest, therefore not in
// pctx.dispatched, therefore its OnFinish never runs either.
func TestRunFinish_OffPolicyPluginNotDispatched(t *testing.T) {
	var offRan atomic.Bool
	off := newFinisher("off-plugin", func(_ context.Context, _ *Context) {
		offRan.Store(true)
	})
	on := newFinisher("on-plugin", nil)
	p, err := New([]Plugin{off, on}, WithPolicies(ErrorPolicyOff, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if offRan.Load() {
		t.Error("off-policy plugin OnFinish ran; should have been skipped (never dispatched in Run)")
	}
}

// TestRunFinish_NonFinisherSkipped verifies plugins that ran OnRequest
// but don't implement Finisher are silently skipped — no panic, no
// effect. Typical mix: stateful plugin + stateless parser in same
// chain.
func TestRunFinish_NonFinisherSkipped(t *testing.T) {
	var finisherRan atomic.Bool
	parser := &stubPlugin{name: "parser"} // doesn't implement Finisher
	f := newFinisher("stateful", func(_ context.Context, _ *Context) {
		finisherRan.Store(true)
	})
	p, err := New([]Plugin{parser, f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if !finisherRan.Load() {
		t.Error("Finisher-implementing plugin didn't run")
	}
}

// TestRunFinish_RecordDroppedDuringFinish verifies pctx.Record called
// from within OnFinish is a no-op (SessionEvent frozen), not a panic
// or a silent append.
func TestRunFinish_RecordDroppedDuringFinish(t *testing.T) {
	f := newFinisher("f", func(_ context.Context, pctx *Context) {
		pctx.Record(Invocation{Action: ActionObserve, Reason: "should_be_dropped"})
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	// No Invocation should have landed from OnFinish.
	if pctx.Extensions.Invocations != nil {
		for _, inv := range pctx.Extensions.Invocations.Inbound {
			if inv.Reason == "should_be_dropped" {
				t.Errorf("OnFinish-emitted Invocation leaked to session (inbound): %+v", inv)
			}
		}
		for _, inv := range pctx.Extensions.Invocations.Outbound {
			if inv.Reason == "should_be_dropped" {
				t.Errorf("OnFinish-emitted Invocation leaked to session (outbound): %+v", inv)
			}
		}
	}
}

// TestRunFinish_SetBodyDroppedDuringFinish verifies pctx.SetBody and
// pctx.SetResponseBody called from within OnFinish are no-ops (response
// already on the wire) — neither panic nor mutate pctx state.
func TestRunFinish_SetBodyDroppedDuringFinish(t *testing.T) {
	f := newFinisher("f", func(_ context.Context, pctx *Context) {
		pctx.SetBody([]byte("should-not-apply"))
		pctx.SetResponseBody([]byte("also-not-apply"))
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	original := []byte("original-req")
	originalResp := []byte("original-resp")
	pctx := &Context{Body: original, ResponseBody: originalResp}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if string(pctx.Body) != "original-req" {
		t.Errorf("Body mutated during OnFinish = %q, want unchanged", pctx.Body)
	}
	if string(pctx.ResponseBody) != "original-resp" {
		t.Errorf("ResponseBody mutated during OnFinish = %q, want unchanged", pctx.ResponseBody)
	}
	if pctx.BodyMutated() {
		t.Error("BodyMutated() true after OnFinish-only mutation; should have been suppressed")
	}
	if pctx.ResponseBodyMutated() {
		t.Error("ResponseBodyMutated() true after OnFinish-only mutation; should have been suppressed")
	}
}

// TestRunFinish_DoubleCallGuard verifies the second RunFinish on a
// pctx is rejected with a WARN and early-returns, so a buggy listener
// (two defers registered, refactor accident) doesn't double-release
// Finisher state.
func TestRunFinish_DoubleCallGuard(t *testing.T) {
	var n atomic.Int32
	f := newFinisher("f", func(_ context.Context, _ *Context) {
		n.Add(1)
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})
	if got := n.Load(); got != 1 {
		t.Errorf("OnFinish fired %d times, want 1 (second RunFinish must no-op)", got)
	}
}

// TestRunFinish_CtxValuePropagation verifies OnFinish sees values
// from the caller's ctx (WithoutCancel preserves values), even though
// cancellation is detached. Uses a per-test context key.
func TestRunFinish_CtxValuePropagation(t *testing.T) {
	type key struct{}
	var seen any
	f := newFinisher("f", func(ctx context.Context, _ *Context) {
		seen = ctx.Value(key{})
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	parent := context.WithValue(context.Background(), key{}, "req-abc-123")
	// Cancel parent to prove cancellation is detached.
	cancelCtx, cancel := context.WithCancel(parent)
	cancel()
	p.Run(context.Background(), pctx)
	p.RunFinish(cancelCtx, pctx, Outcome{FinalAction: OutcomeAllow})
	if seen != "req-abc-123" {
		t.Errorf("OnFinish ctx value = %v, want req-abc-123 (WithoutCancel must preserve values)", seen)
	}
}

// TestRun_StampsRejectingPlugin verifies Pipeline.Run populates
// pctx.RejectingPlugin() on an enforce-policy deny but leaves it
// empty on shadow-mode denies and on allow paths.
func TestRun_StampsRejectingPlugin(t *testing.T) {
	tests := []struct {
		name     string
		onReq    func(*Context) Action
		policy   ErrorPolicy
		wantName string
	}{
		{
			name:     "allow: RejectingPlugin empty",
			onReq:    func(*Context) Action { return Action{Type: Continue} },
			policy:   ErrorPolicyEnforce,
			wantName: "",
		},
		{
			name: "enforce deny: RejectingPlugin populated",
			onReq: func(*Context) Action {
				return Action{Type: Reject, Violation: &Violation{Code: "t", Reason: "r"}}
			},
			policy:   ErrorPolicyEnforce,
			wantName: "denier",
		},
		{
			name: "observe deny (shadow): RejectingPlugin empty",
			onReq: func(*Context) Action {
				return Action{Type: Reject, Violation: &Violation{Code: "t", Reason: "r"}}
			},
			policy:   ErrorPolicyObserve,
			wantName: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubPlugin{name: "denier", onReq: func(_ context.Context, pctx *Context) Action {
				return tc.onReq(pctx)
			}}
			p, err := New([]Plugin{s}, WithPolicies(tc.policy))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			pctx := &Context{}
			p.Run(context.Background(), pctx)
			if got := pctx.RejectingPlugin(); got != tc.wantName {
				t.Errorf("RejectingPlugin = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestOutcomeFromContext_PrefersRejectingPlugin verifies the helper
// reads pctx.RejectingPlugin() first, so a plugin that returns Reject
// without calling pctx.Record is still classified as OutcomeDeny.
func TestOutcomeFromContext_PrefersRejectingPlugin(t *testing.T) {
	s := &stubPlugin{name: "no-record-denier", onReq: func(_ context.Context, _ *Context) Action {
		// Return Reject without recording. Pre-fix this path
		// classified as OutcomeError (StatusCode 0) or OutcomeAllow
		// (StatusCode > 0) depending on downstream state.
		return Action{Type: Reject, Violation: &Violation{Code: "t", Reason: "r"}}
	}}
	p, err := New([]Plugin{s})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	out := OutcomeFromContext(pctx)
	if out.FinalAction != OutcomeDeny {
		t.Errorf("FinalAction = %q, want deny (framework-stamped rejectingPlugin must drive classification)", out.FinalAction)
	}
	if out.DenyingPlugin != "no-record-denier" {
		t.Errorf("DenyingPlugin = %q, want %q", out.DenyingPlugin, "no-record-denier")
	}
}

// TestOutcomeFromContext covers the listener-facing derivation helper:
// deny Invocations win, StatusCode 0 means Error, non-zero + no deny
// means Allow. Shadow denials don't count.
func TestOutcomeFromContext(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*Context)
		wantAct  OutcomeAction
		wantDeny string
	}{
		{
			name: "no invocations, status 200 → allow",
			setup: func(pctx *Context) {
				pctx.StatusCode = 200
			},
			wantAct: OutcomeAllow,
		},
		{
			name: "no invocations, status 0 → error",
			setup: func(pctx *Context) {
				pctx.StatusCode = 0
			},
			wantAct: OutcomeError,
		},
		{
			name: "no invocations, upstream 502 → allow (pipeline didn't deny)",
			setup: func(pctx *Context) {
				pctx.StatusCode = 502
			},
			wantAct: OutcomeAllow,
		},
		{
			name: "inbound deny invocation → deny + plugin name",
			setup: func(pctx *Context) {
				pctx.StatusCode = 401
				pctx.Extensions.Invocations = &Invocations{
					Inbound: []Invocation{
						{Plugin: "allow-plugin", Action: ActionAllow},
						{Plugin: "jwt-validation", Action: ActionDeny, Reason: "bad_token"},
					},
				}
			},
			wantAct:  OutcomeDeny,
			wantDeny: "jwt-validation",
		},
		{
			name: "outbound deny wins over inbound deny (most recent)",
			setup: func(pctx *Context) {
				pctx.StatusCode = 503
				pctx.Extensions.Invocations = &Invocations{
					Inbound:  []Invocation{{Plugin: "inbound-gate", Action: ActionDeny}},
					Outbound: []Invocation{{Plugin: "token-exchange", Action: ActionDeny, Reason: "idp_down"}},
				}
			},
			wantAct:  OutcomeDeny,
			wantDeny: "token-exchange",
		},
		{
			name: "shadow deny ignored (observe mode — not an actual deny)",
			setup: func(pctx *Context) {
				pctx.StatusCode = 200
				pctx.Extensions.Invocations = &Invocations{
					Inbound: []Invocation{
						{Plugin: "canary", Action: ActionDeny, Shadow: true},
					},
				}
			},
			wantAct: OutcomeAllow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pctx := &Context{}
			tc.setup(pctx)
			out := OutcomeFromContext(pctx)
			if out.FinalAction != tc.wantAct {
				t.Errorf("FinalAction = %q, want %q", out.FinalAction, tc.wantAct)
			}
			if out.DenyingPlugin != tc.wantDeny {
				t.Errorf("DenyingPlugin = %q, want %q", out.DenyingPlugin, tc.wantDeny)
			}
			if out.StatusCode != pctx.StatusCode {
				t.Errorf("StatusCode = %d, want %d", out.StatusCode, pctx.StatusCode)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
