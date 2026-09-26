package pipeline

import (
	"context"
	"testing"
)

// streamingStubPlugin embeds stubPlugin with a StreamingResponder
// implementation. Tests opt in by setting onFrame.
type streamingStubPlugin struct {
	stubPlugin
	onFrame func(ctx context.Context, pctx *Context, frame []byte, last bool) Action
}

func (s *streamingStubPlugin) OnResponseFrame(ctx context.Context, pctx *Context, frame []byte, last bool) Action {
	if s.onFrame != nil {
		return s.onFrame(ctx, pctx, frame, last)
	}
	return Action{Type: Continue}
}

func TestRunResponseFrame_DispatchOrder_ReverseDeclaration(t *testing.T) {
	var order []string
	p1 := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "first"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			order = append(order, "first")
			return Action{Type: Continue}
		},
	}
	p2 := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "second"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			order = append(order, "second")
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	action := pipe.RunResponseFrame(context.Background(), pctx, []byte("frame"), false)
	if action.Type != Continue {
		t.Errorf("action = %v, want Continue", action.Type)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("dispatch order = %v, want reverse declaration [second first]", order)
	}
}

func TestRunResponseFrame_NonStreamingPluginsSkipped(t *testing.T) {
	called := false
	p := &stubPlugin{
		name: "non-streaming",
		onResp: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	pipe.RunResponseFrame(context.Background(), pctx, []byte("frame"), false)
	if called {
		t.Error("RunResponseFrame called OnResponse on a non-streaming plugin")
	}
}

func TestRunResponseFrame_LastFlag(t *testing.T) {
	var seenLast []bool
	p := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "agg"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, last bool) Action {
			seenLast = append(seenLast, last)
			return Action{Type: Continue}
		},
	}
	pipe, _ := New([]Plugin{p})
	pctx := &Context{}
	pipe.RunResponseFrame(context.Background(), pctx, []byte("a"), false)
	pipe.RunResponseFrame(context.Background(), pctx, []byte("b"), false)
	pipe.RunResponseFrame(context.Background(), pctx, nil, true)
	if len(seenLast) != 3 {
		t.Fatalf("got %d calls, want 3", len(seenLast))
	}
	if seenLast[0] || seenLast[1] || !seenLast[2] {
		t.Errorf("last flags = %v, want [false false true]", seenLast)
	}
}

func TestRunResponseFrame_RejectStops(t *testing.T) {
	var dispatched []string
	p1 := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "first"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			dispatched = append(dispatched, "first")
			return Action{Type: Continue}
		},
	}
	p2 := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "rejecter"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			dispatched = append(dispatched, "rejecter")
			return Deny("test.reject", "denied")
		},
	}
	// Reverse-order dispatch means rejecter runs first (last in slice).
	pipe, _ := New([]Plugin{p1, p2})
	pctx := &Context{}
	action := pipe.RunResponseFrame(context.Background(), pctx, []byte("x"), false)
	if action.Type != Reject {
		t.Errorf("action = %v, want Reject", action.Type)
	}
	if len(dispatched) != 1 || dispatched[0] != "rejecter" {
		t.Errorf("dispatched = %v, want [rejecter] only (reject stops chain)", dispatched)
	}
}

func TestHasStreamingResponders(t *testing.T) {
	plain := &stubPlugin{name: "plain"}
	stream := &streamingStubPlugin{stubPlugin: stubPlugin{name: "stream"}}

	pipe1, _ := New([]Plugin{plain})
	if pipe1.HasStreamingResponders() {
		t.Error("pipe with only plain plugin reports HasStreamingResponders=true")
	}
	pipe2, _ := New([]Plugin{plain, stream})
	if !pipe2.HasStreamingResponders() {
		t.Error("pipe with streaming plugin reports HasStreamingResponders=false")
	}
}

func TestRunResponseFrame_ContextCancelled(t *testing.T) {
	called := false
	p := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "p"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, _ := New([]Plugin{p})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pctx := &Context{}
	action := pipe.RunResponseFrame(ctx, pctx, nil, true)
	if action.Type != Reject {
		t.Errorf("action = %v, want Reject on cancelled ctx", action.Type)
	}
	if called {
		t.Error("plugin OnResponseFrame called on cancelled ctx")
	}
}

func TestRunResponseFrame_OffPolicySkipped(t *testing.T) {
	called := false
	p := &streamingStubPlugin{
		stubPlugin: stubPlugin{name: "off"},
		onFrame: func(_ context.Context, _ *Context, _ []byte, _ bool) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, _ := New([]Plugin{p}, WithPolicies(ErrorPolicyOff))
	pctx := &Context{}
	pipe.RunResponseFrame(context.Background(), pctx, []byte("x"), false)
	if called {
		t.Error("off-policy plugin's OnResponseFrame was called")
	}
}
