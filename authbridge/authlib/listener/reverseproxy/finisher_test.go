package reverseproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/plugintesting"
)

// waitSeen polls b until it's true, up to ~1s. OnFinish runs in the
// reverse-proxy handler's deferred RunFinish, which fires only after the
// streamed response (NewServer sets FlushInterval=-1) has already reached the
// client — so http.Get can return before OnFinish has run. Poll rather than
// read immediately, which otherwise races (flaky under -race).
func waitSeen(b *atomic.Bool) bool {
	for i := 0; i < 1000; i++ {
		if b.Load() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// finisherStub is a minimal Plugin + Finisher used by these tests to
// observe OnFinish dispatch and the Outcome the listener derived.
type finisherStub struct {
	name    string
	onReq   func(*pipeline.Context) pipeline.Action
	seen    atomic.Bool
	outcome atomic.Pointer[pipeline.Outcome]
}

func (p *finisherStub) Name() string { return p.name }
func (p *finisherStub) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *finisherStub) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.onReq != nil {
		return p.onReq(pctx)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *finisherStub) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *finisherStub) OnFinish(_ context.Context, pctx *pipeline.Context) {
	if o := pctx.Outcome(); o != nil {
		cp := *o
		p.outcome.Store(&cp)
	}
	// Store seen LAST so a reader that observes seen==true is guaranteed to
	// also see the outcome that was set just above.
	p.seen.Store(true)
}

func pipelineWith(t *testing.T, plugins ...pipeline.Plugin) *pipeline.Holder {
	t.Helper()
	p, err := plugintesting.BuildPipeline(plugins)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	return pipeline.NewHolder(p)
}

// TestReverseProxy_Finisher_Allow verifies OnFinish fires with
// OutcomeAllow on the happy path.
func TestReverseProxy_Finisher_Allow(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	f := &finisherStub{name: "f-allow"}
	srv, err := NewServer(pipelineWith(t, f), nil, backend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !waitSeen(&f.seen) {
		t.Fatal("OnFinish did not fire")
	}
	o := f.outcome.Load()
	if o == nil {
		t.Fatal("Outcome was nil in OnFinish")
	}
	if o.FinalAction != pipeline.OutcomeAllow {
		t.Errorf("FinalAction = %q, want allow", o.FinalAction)
	}
	if o.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", o.StatusCode)
	}
	if o.DenyingPlugin != "" {
		t.Errorf("DenyingPlugin = %q, want empty", o.DenyingPlugin)
	}
}

// TestReverseProxy_Finisher_Deny verifies OnFinish fires on the plugin
// that denied AND on earlier plugins whose OnRequest ran. Both see
// OutcomeDeny with the correct DenyingPlugin attribution.
func TestReverseProxy_Finisher_Deny(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be called on deny")
	}))
	defer backend.Close()

	before := &finisherStub{name: "before-deny"}
	denier := &finisherStub{
		name: "denier",
		onReq: func(pctx *pipeline.Context) pipeline.Action {
			pctx.Record(pipeline.Invocation{
				Plugin: "denier",
				Action: pipeline.ActionDeny,
				Reason: "test_deny",
			})
			return pipeline.Action{
				Type:      pipeline.Reject,
				Violation: &pipeline.Violation{Status: 403, Code: "test.deny", Reason: "denied for test"},
			}
		},
	}
	after := &finisherStub{name: "after-deny"}

	srv, err := NewServer(pipelineWith(t, before, denier, after), nil, backend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTP status = %d, want 403", resp.StatusCode)
	}

	if !waitSeen(&before.seen) {
		t.Error("before-deny.OnFinish should have fired (OnRequest ran before denial)")
	}
	if !waitSeen(&denier.seen) {
		t.Error("denier.OnFinish should have fired (OnRequest ran and produced the deny)")
	}
	// before/denier have fired, so the single RunFinish dispatch is complete;
	// after-deny was never dispatched (its OnRequest never ran), so its
	// OnFinish must not have fired.
	if after.seen.Load() {
		t.Error("after-deny.OnFinish should NOT have fired (OnRequest never ran)")
	}

	// Both should report FinalAction=Deny, DenyingPlugin=denier.
	for _, stub := range []*finisherStub{before, denier} {
		o := stub.outcome.Load()
		if o == nil {
			t.Errorf("%s: outcome nil", stub.name)
			continue
		}
		if o.FinalAction != pipeline.OutcomeDeny {
			t.Errorf("%s: FinalAction = %q, want deny", stub.name, o.FinalAction)
		}
		if o.DenyingPlugin != "denier" {
			t.Errorf("%s: DenyingPlugin = %q, want %q", stub.name, o.DenyingPlugin, "denier")
		}
	}
}
