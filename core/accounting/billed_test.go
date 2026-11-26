package accounting

import (
	"context"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
)

// billedOperator sends the client body as it is, and says through SetBilledBody that the provider
// bills billed for it: the shape of context-guru's tool deferral, which keeps a deferred tool in
// the bytes sent (rossoctl/cortex#2150).
type billedOperator struct {
	billed   []byte
	unplaced bool
	ops      []accountant.Operation
}

func (o *billedOperator) Name() string { return "billed-op" }
func (o *billedOperator) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesRequestBody: true}
}
func (o *billedOperator) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SetBilledBody(func([]byte) ([]byte, bool) { return o.billed, o.unplaced })
	pctx.ReportOps(o.ops...)
	return pipeline.Action{Type: pipeline.Continue}
}
func (o *billedOperator) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func billedRequest(t *testing.T, op *billedOperator) (accountant.Result, bool) {
	t.Helper()
	h := newHost(t)
	p, err := pipeline.New([]pipeline.Plugin{op}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := runOn(t, p, "s1", "/v1/messages", clientBody("hi"), 200, coldUsage())
	return h.Compute(pctx)
}

// The library reads the billed body, not the bytes sent: the op that explains the billed body's
// shrink passes the sum check, although the body sent did not shrink.
func TestHost_RecordsTheBilledBody(t *testing.T) {
	res, ok := billedRequest(t, &billedOperator{billed: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}})
	if !ok {
		t.Fatal("Compute gave no Result")
	}
	for _, c := range res.Checks {
		if c.Check == accountant.CheckSum {
			t.Fatalf("sum check failed: %s", c.Detail)
		}
	}
	if s := res.Ops[0].Saved; s.Input+s.CacheWrite5m+s.CacheWrite1h+s.CacheRead == 0 {
		t.Fatalf("the op saved nothing: %+v", res.Ops[0])
	}
}

// A change the hook cannot describe books no figure (deferral_unplaced).
func TestHost_UnplacedBilledBodyBooksNoFigure(t *testing.T) {
	if _, ok := billedRequest(t, &billedOperator{billed: clientBody("hi"), unplaced: true}); ok {
		t.Fatal("an unplaced change booked a figure")
	}
}
