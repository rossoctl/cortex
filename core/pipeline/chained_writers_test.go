package pipeline

import (
	"context"
	"strings"
	"testing"
)

// appender is a request-body mutator that appends suffix to whatever body it is
// handed, so a test can tell from the final bytes which writers ran and in what
// order.
func appender(name, suffix string) *stubPlugin {
	return &stubPlugin{
		name: name,
		caps: PluginCapabilities{WritesRequestBody: true},
		onReq: func(_ context.Context, pctx *Context) Action {
			pctx.SetBody(append(append([]byte{}, pctx.Body...), suffix...))
			return Action{Type: Continue}
		},
	}
}

// Request mutators chain: each sees pctx.Body as the one before it left it, and
// the body the listener sends is the last one's.
func TestNew_ChainsRequestMutators(t *testing.T) {
	var seenBySecond string
	second := appender("second", "+b")
	inner := second.onReq
	second.onReq = func(ctx context.Context, pctx *Context) Action {
		seenBySecond = string(pctx.Body)
		return inner(ctx, pctx)
	}
	p, err := New([]Plugin{
		&stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}},
		appender("first", "+a"),
		second,
	})
	if err != nil {
		t.Fatalf("New refused two request mutators after a reader: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	if seenBySecond != "x+a" {
		t.Errorf("second mutator saw %q, want the first one's output x+a", seenBySecond)
	}
	if string(pctx.Body) != "x+a+b" || !pctx.BodyMutated() {
		t.Errorf("Body = %q, BodyMutated = %v; want x+a+b, true", pctx.Body, pctx.BodyMutated())
	}
}

// The reader rule is what the one-mutator rule protected, and it holds with any
// number of mutators: a reader after any of them fails, naming the first.
func TestNew_RejectsAReaderAfterAnyRequestMutator(t *testing.T) {
	reader := &stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}}
	for name, chain := range map[string][]Plugin{
		"after the only one": {appender("first", "+a"), reader},
		"after both":         {appender("first", "+a"), appender("second", "+b"), reader},
		"between the two":    {appender("first", "+a"), reader, appender("second", "+b")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(chain)
			if err == nil {
				t.Fatal("New accepted a body reader after a mutator")
			}
			if !strings.Contains(err.Error(), `plugin "parser" reads body after mutator "first"`) {
				t.Errorf("err = %v, want it to name the reader and the first mutator", err)
			}
		})
	}
}

// Under observe a mutator's write is a no-op, so the next mutator sees the body
// as it was.
func TestNew_AnObservedMutatorLeavesTheNextOneTheUnmodifiedBody(t *testing.T) {
	var seenBySecond string
	second := appender("second", "+b")
	inner := second.onReq
	second.onReq = func(ctx context.Context, pctx *Context) Action {
		seenBySecond = string(pctx.Body)
		return inner(ctx, pctx)
	}
	p, err := New([]Plugin{appender("first", "+a"), second}, WithPolicies(ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	if seenBySecond != "x" || string(pctx.Body) != "x+b" {
		t.Errorf("second saw %q and the body is %q; want x and x+b", seenBySecond, pctx.Body)
	}
}

// responseAppender is a response-body mutator that appends suffix to whatever
// response it is handed, so a test can tell from the final bytes which writers
// ran and in what order.
func responseAppender(name, suffix string) *stubPlugin {
	return &stubPlugin{
		name: name,
		caps: PluginCapabilities{WritesResponseBody: true},
		onResp: func(_ context.Context, pctx *Context) Action {
			pctx.SetResponseBody(append(append([]byte{}, pctx.ResponseBody...), suffix...))
			return Action{Type: Continue}
		},
	}
}

// runResponseLikeAListener dispatches the response pass the way every listener's
// buffered path does: RunResponse, then the parsers' terminal frame carrying the
// body as the writers left it.
func runResponseLikeAListener(t *testing.T, p *Pipeline, pctx *Context) {
	t.Helper()
	if a := p.RunResponse(context.Background(), pctx); a.Type != Continue {
		t.Fatalf("RunResponse = %v, want Continue", a.Type)
	}
	if a := p.RunResponseFrame(context.Background(), pctx, pctx.ResponseBody, true); a.Type != Continue {
		t.Fatalf("RunResponseFrame = %v, want Continue", a.Type)
	}
}

// Response mutators chain. The response pass runs in reverse, so the writer
// placed last runs first and the one before it sees its output. Readers placed
// ahead of both — a whole-body reader and a per-frame one, as the parsers are —
// see the response the client receives, and the record names both writers in
// the order they ran.
func TestNew_ChainsResponseMutators(t *testing.T) {
	var seenByFirst, seenByReader, seenByParser string
	first := responseAppender("first", "+a")
	inner := first.onResp
	first.onResp = func(ctx context.Context, pctx *Context) Action {
		seenByFirst = string(pctx.ResponseBody)
		return inner(ctx, pctx)
	}
	reader := &stubPlugin{
		name: "reader",
		caps: PluginCapabilities{ReadsBody: true},
		onResp: func(_ context.Context, pctx *Context) Action {
			seenByReader = string(pctx.ResponseBody)
			return Action{Type: Continue}
		},
	}
	parser := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}},
		onFrame: func(_ context.Context, _ *Context, frame []byte, _ bool) Action {
			seenByParser = string(frame)
			return Action{Type: Continue}
		},
	}
	p, err := New([]Plugin{parser, reader, first, responseAppender("second", "+b")})
	if err != nil {
		t.Fatalf("New refused two response mutators after the readers: %v", err)
	}
	pctx := &Context{Direction: Outbound, ResponseBody: []byte("x")}
	runResponseLikeAListener(t, p, pctx)

	if seenByFirst != "x+b" {
		t.Errorf("first saw %q, want second's output x+b — the response pass runs in reverse", seenByFirst)
	}
	if string(pctx.ResponseBody) != "x+b+a" || !pctx.ResponseBodyMutated() {
		t.Errorf("ResponseBody = %q, ResponseBodyMutated = %v; want x+b+a, true", pctx.ResponseBody, pctx.ResponseBodyMutated())
	}
	if seenByReader != "x+b+a" || seenByParser != "x+b+a" {
		t.Errorf("reader saw %q, parser saw %q; want both to see the response the client receives, x+b+a", seenByReader, seenByParser)
	}
	assertMutation(t, bodyMutation(t, pctx), "response", "x", "x+b+a", "second", "first")
}

// Under observe a response mutator's write is a no-op: the writer that runs after
// it sees the response as it was, and the record names only the writers whose
// writes took effect.
func TestNew_AnObservedResponseMutatorLeavesTheNextOneTheBody(t *testing.T) {
	var seenByFirst string
	first := responseAppender("first", "+a")
	inner := first.onResp
	first.onResp = func(ctx context.Context, pctx *Context) Action {
		seenByFirst = string(pctx.ResponseBody)
		return inner(ctx, pctx)
	}
	p, err := New(
		[]Plugin{first, responseAppender("observed", "+m"), responseAppender("second", "+b")},
		WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve, ErrorPolicyEnforce),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, ResponseBody: []byte("x")}
	runResponseLikeAListener(t, p, pctx)

	if seenByFirst != "x+b" {
		t.Errorf("first saw %q, want x+b — the observed write must change nothing", seenByFirst)
	}
	if string(pctx.ResponseBody) != "x+b+a" {
		t.Errorf("ResponseBody = %q, want x+b+a", pctx.ResponseBody)
	}
	assertMutation(t, bodyMutation(t, pctx), "response", "x", "x+b+a", "second", "first")
}

// A plugin may write both bodies — cpex does — and share a chain with a
// response-only writer such as sparc. Each direction chains on its own.
func TestNew_ChainsABothDirectionWriterWithAResponseWriter(t *testing.T) {
	both := &stubPlugin{
		name: "cpex",
		caps: PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true},
		onReq: func(_ context.Context, pctx *Context) Action {
			pctx.SetBody(append(append([]byte{}, pctx.Body...), "+A"...))
			return Action{Type: Continue}
		},
		onResp: func(_ context.Context, pctx *Context) Action {
			pctx.SetResponseBody(append(append([]byte{}, pctx.ResponseBody...), "+a"...))
			return Action{Type: Continue}
		},
	}
	p, err := New([]Plugin{both, responseAppender("sparc", "+b")})
	if err != nil {
		t.Fatalf("New refused [cpex, sparc]: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("q")}
	if a := p.Run(context.Background(), pctx); a.Type != Continue {
		t.Fatalf("Run = %v, want Continue", a.Type)
	}
	pctx.ResponseBody = []byte("r")
	runResponseLikeAListener(t, p, pctx)

	if string(pctx.Body) != "q+A" {
		t.Errorf("Body = %q, want q+A", pctx.Body)
	}
	if string(pctx.ResponseBody) != "r+b+a" {
		t.Errorf("ResponseBody = %q, want r+b+a — sparc runs first on the response", pctx.ResponseBody)
	}
}

// bodyMutation is the framework's record of the rewrites on pctx.
func bodyMutation(t *testing.T, pctx *Context) bodyMutationEvent {
	t.Helper()
	raw, ok := pctx.Extensions.Custom["body-mutation"+PluginEventSuffix]
	if !ok {
		t.Fatalf("no body-mutation event; keys: %v", keys(pctx.Extensions.Custom))
	}
	ev, ok := raw.(bodyMutationEvent)
	if !ok {
		t.Fatalf("event type = %T, want bodyMutationEvent", raw)
	}
	return ev
}

// assertMutation fails unless ev describes before → after, written by plugins in
// that order, with plugin naming the last of them.
func assertMutation(t *testing.T, ev bodyMutationEvent, phase, before, after string, plugins ...string) {
	t.Helper()
	if ev.Phase != phase {
		t.Errorf("phase = %q, want %q", ev.Phase, phase)
	}
	if ev.LengthBefore != len(before) || ev.SHA256Before != hashHex([]byte(before)) {
		t.Errorf("before = %d bytes %s, want %q", ev.LengthBefore, ev.SHA256Before, before)
	}
	if ev.LengthAfter != len(after) || ev.SHA256After != hashHex([]byte(after)) {
		t.Errorf("after = %d bytes %s, want %q", ev.LengthAfter, ev.SHA256After, after)
	}
	if strings.Join(ev.Plugins, ",") != strings.Join(plugins, ",") || ev.Plugin != plugins[len(plugins)-1] {
		t.Errorf("plugin = %q, plugins = %v; want %q and %v", ev.Plugin, ev.Plugins, plugins[len(plugins)-1], plugins)
	}
}

// With two writers the record keeps the bytes the client sent as before, the
// bytes sent upstream as after, and both writers in order. Each writer's own
// modify/body_rewritten stays on the timeline.
func TestBodyMutation_RecordsTheClientsBytesAndEveryWriter(t *testing.T) {
	p := mustBuild(t, appender("first", "+a"), appender("second", "+b"))
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+a+b", "first", "second")
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 2 || invs[0].Plugin != "first" || invs[1].Plugin != "second" ||
		invs[0].Reason != "body_rewritten" || invs[1].Reason != "body_rewritten" {
		t.Errorf("invocations = %+v, want one body_rewritten per writer, in order", invs)
	}
}

// A shadow write sent nothing upstream, so it must not displace the record of a
// write that did.
func TestBodyMutation_AShadowWriteLeavesAnAppliedRecordAlone(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")},
		WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+a", "first")
}

// A shadow write publishes its would-be record, as a lone observed writer always
// has, until a write takes effect and replaces it.
func TestBodyMutation_AnAppliedWriteReplacesAShadowRecord(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")},
		WithPolicies(ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+b", "second")
}

// The response side keeps its own record: its before is the response the
// upstream sent, not the request the client did.
func TestBodyMutation_TheResponseRecordStartsFromTheResponse(t *testing.T) {
	c := &Context{Direction: Outbound, Body: []byte("req")}
	c.SetCurrentPlugin("pruner", InvocationPhaseRequest)
	c.SetBody([]byte("req+a"))
	c.ResponseBody = []byte("resp")
	c.SetCurrentPlugin("filter", InvocationPhaseResponse)
	c.SetResponseBody([]byte("resp+f"))

	assertMutation(t, bodyMutation(t, c), "response", "resp", "resp+f", "filter")
}

// reporter is appender that keeps what its own SetBody call answered.
func reporter(name, suffix string, applied *bool) *stubPlugin {
	return &stubPlugin{
		name: name,
		caps: PluginCapabilities{WritesRequestBody: true},
		onReq: func(_ context.Context, pctx *Context) Action {
			*applied = pctx.SetBody(append(append([]byte{}, pctx.Body...), suffix...))
			return Action{Type: Continue}
		},
	}
}

// SetBody answers for the call it was given: true when those bytes took effect,
// false for a write under on_error: observe. BodyMutated is request-wide, so after
// an earlier writer took effect it says true for an observed writer whose own
// bytes went nowhere — which is why a writer must read its own result.
func TestSetBody_SaysWhetherThisWriteTookEffect(t *testing.T) {
	enforce, observe := ErrorPolicyEnforce, ErrorPolicyObserve
	for name, tc := range map[string]struct {
		policies              []ErrorPolicy
		wantFirst, wantSecond bool
		wantBody              string
	}{
		"both enforced":    {[]ErrorPolicy{enforce, enforce}, true, true, "x+a+b"},
		"second observed":  {[]ErrorPolicy{enforce, observe}, true, false, "x+a"},
		"first observed":   {[]ErrorPolicy{observe, enforce}, false, true, "x+b"},
		"neither enforced": {[]ErrorPolicy{observe, observe}, false, false, "x"},
	} {
		t.Run(name, func(t *testing.T) {
			var first, second bool
			p, err := New([]Plugin{reporter("first", "+a", &first), reporter("second", "+b", &second)},
				WithPolicies(tc.policies...))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			pctx := &Context{Direction: Outbound, Body: []byte("x")}
			p.Run(context.Background(), pctx)

			if first != tc.wantFirst || second != tc.wantSecond {
				t.Errorf("SetBody answered first=%v second=%v, want %v and %v",
					first, second, tc.wantFirst, tc.wantSecond)
			}
			if string(pctx.Body) != tc.wantBody {
				t.Errorf("Body = %q, want %q", pctx.Body, tc.wantBody)
			}
			if name == "second observed" && !pctx.BodyMutated() {
				t.Error("BodyMutated = false; the first writer's bytes took effect, and it answers for the request")
			}
		})
	}
}

// The response side answers the same way.
func TestSetResponseBody_SaysWhetherThisWriteTookEffect(t *testing.T) {
	for name, tc := range map[string]struct {
		policy   ErrorPolicy
		want     bool
		wantBody string
	}{
		"enforced": {ErrorPolicyEnforce, true, "resp+f"},
		"observed": {ErrorPolicyObserve, false, "resp"},
	} {
		t.Run(name, func(t *testing.T) {
			var applied bool
			filter := &stubPlugin{
				name: "filter",
				caps: PluginCapabilities{WritesResponseBody: true},
				onResp: func(_ context.Context, pctx *Context) Action {
					applied = pctx.SetResponseBody(append(append([]byte{}, pctx.ResponseBody...), "+f"...))
					return Action{Type: Continue}
				},
			}
			p, err := New([]Plugin{filter}, WithPolicies(tc.policy))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			pctx := &Context{Direction: Outbound, ResponseBody: []byte("resp")}
			p.RunResponse(context.Background(), pctx)

			if applied != tc.want || string(pctx.ResponseBody) != tc.wantBody {
				t.Errorf("SetResponseBody answered %v and the body is %q; want %v and %q",
					applied, pctx.ResponseBody, tc.want, tc.wantBody)
			}
		})
	}
}

// A write dropped in OnFinish took no effect, and both calls say so.
func TestSetBody_AWriteDroppedInOnFinishSaysSo(t *testing.T) {
	reqApplied, respApplied := true, true
	f := newFinisher("f", func(_ context.Context, pctx *Context) {
		reqApplied = pctx.SetBody([]byte("late"))
		respApplied = pctx.SetResponseBody([]byte("late"))
	})
	p, err := New([]Plugin{f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Body: []byte("req"), ResponseBody: []byte("resp")}
	p.Run(context.Background(), pctx)
	p.RunFinish(context.Background(), pctx, Outcome{FinalAction: OutcomeAllow})

	if reqApplied || respApplied {
		t.Errorf("SetBody answered %v and SetResponseBody %v in OnFinish; both were dropped", reqApplied, respApplied)
	}
}

// [enforce, observe, enforce]: the shadow write in the middle sent nothing, so the
// record names the two writers whose bytes went upstream, and its after is theirs.
func TestBodyMutation_AShadowBetweenTwoAppliedWritesIsLeftOut(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b"), appender("third", "+c")},
		WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+a+c", "first", "third")
}

// Two shadow writes in a row: neither took effect, so the record is the last one's
// would-be rewrite of the client's body, naming only it, and both invocations are
// shadows.
func TestBodyMutation_TwoShadowWritesRecordTheLastOnesWouldBeRewrite(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")},
		WithPolicies(ErrorPolicyObserve, ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+b", "second")
	if pctx.BodyMutated() || string(pctx.Body) != "x" {
		t.Errorf("BodyMutated = %v, Body = %q; want false and the client's x", pctx.BodyMutated(), pctx.Body)
	}
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 2 || !invs[0].Shadow || !invs[1].Shadow {
		t.Errorf("invocations = %+v, want two shadow body_rewritten records", invs)
	}
}

// Settlement calibrates on (len(pctx.Body), pctx.BodyMutated()) as the body sent:
// after the chain, Body is what the writes that took effect left — the last one's,
// however many wrote — and BodyMutated is false when none did, a shadow write
// counting for nothing. The published record cannot stand in for it: it holds a
// shadow's would-be length while nothing has taken effect.
func TestAppliedWrites_LeaveTheBodySent(t *testing.T) {
	enforce, observe := ErrorPolicyEnforce, ErrorPolicyObserve
	for name, tc := range map[string]struct {
		policies []ErrorPolicy
		wantLen  int
		wantOK   bool
	}{
		"both enforced":    {[]ErrorPolicy{enforce, enforce}, len("x+a+b"), true},
		"second observed":  {[]ErrorPolicy{enforce, observe}, len("x+a"), true},
		"first observed":   {[]ErrorPolicy{observe, enforce}, len("x+b"), true},
		"neither enforced": {[]ErrorPolicy{observe, observe}, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")}, WithPolicies(tc.policies...))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			pctx := &Context{Direction: Outbound, Body: []byte("x")}
			p.Run(context.Background(), pctx)

			if n, ok := len(pctx.Body), pctx.BodyMutated(); ok != tc.wantOK || (ok && n != tc.wantLen) {
				t.Errorf("len(Body), BodyMutated = %d, %v; want %d, %v", n, ok, tc.wantLen, tc.wantOK)
			}
		})
	}
}

// A response write publishes over the request's record in Extensions.Custom — one
// key serves both directions — but leaves the request body alone, which is why a
// response-time reader such as settlement reads pctx.Body rather than the
// published map.
func TestTheBodySent_SurvivesAResponseWrite(t *testing.T) {
	c := &Context{Direction: Outbound, Body: []byte("req")}
	c.SetCurrentPlugin("pruner", InvocationPhaseRequest)
	c.SetBody([]byte("req+a"))
	c.ResponseBody = []byte("resp")
	c.SetCurrentPlugin("filter", InvocationPhaseResponse)
	c.SetResponseBody([]byte("a much longer response"))

	if n, ok := len(c.Body), c.BodyMutated(); n != len("req+a") || !ok {
		t.Errorf("len(Body), BodyMutated = %d, %v after a response write; want %d, true", n, ok, len("req+a"))
	}
}
