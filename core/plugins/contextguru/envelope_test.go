package contextguru

import (
	"context"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// rewriter stands in for the plugin's OnRequest: it reports a hand-built apply.Result through
// reportResult, as the plugin does, and forwards its body. The engine is not run: its store would
// make a second run see different state, and these tests are about the envelope, which apply
// changes without an op.
type rewriter struct {
	res apply.Result
}

func (r *rewriter) Name() string { return "context-guru" }
func (r *rewriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesRequestBody: true}
}
func (r *rewriter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if out, changed := reportResult(pctx, r.res); changed {
		pctx.SetBody(out)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (r *rewriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

var (
	bigTool   = `{"name":"Big","description":"` + strings.Repeat("x", 4000) + `","input_schema":{"type":"object"}}`
	smallTool = `{"name":"Read","description":"read a file","input_schema":{"type":"object"}}`
	convo     = `[{"role":"user","content":[{"type":"text","text":"hi` + strings.Repeat(" lorem ipsum dolor", 400) +
		`","cache_control":{"type":"ephemeral"}}]}]`
)

func body(tools, system string) []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` + tools + `],"system":` + system +
		`,"messages":` + convo + `}`)
}

// priced runs client -> forwarded through a Host, a cold first request, and returns the Result
// and the ops reported.
func priced(t *testing.T, client, forwarded []byte) (accountant.Result, []accountant.Operation) {
	t.Helper()
	h, err := accounting.New(accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	pipe, err := pipeline.New([]pipeline.Plugin{&rewriter{res: apply.Result{Body: forwarded, Changed: true}}},
		pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &pipeline.Context{Host: "api.anthropic.com", Path: "/v1/messages", Method: "POST", Body: client, SessionID: "s1"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5"}
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	ops := pctx.ReportedOps()
	pctx.StatusCode = 200
	pctx.Extensions.Inference.InputTokens = 3
	pctx.Extensions.Inference.CacheWriteTokens = len(forwarded) / 4
	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute returned no Result")
	}
	return res, ops
}

func noSumFailure(t *testing.T, res accountant.Result) {
	t.Helper()
	for _, c := range res.Checks {
		if c.Check == accountant.CheckSum {
			t.Errorf("sum check failed: %s", c.Detail)
		}
	}
	// A request-level penalty_unknown (empty OpID) is the library's cold-start caution about the
	// boundary on a thread with no history, not an envelope matter; an op of its own must not be
	// unpriced.
	for _, u := range res.Unpriced {
		if u.OpID != "" {
			t.Errorf("op %s unpriced: %s", u.OpID, u.Reason)
		}
	}
}

// cachesplit splits the system block at its volatile tail, in every preset, and reports no op:
// the plugin reports it from the bodies, as the proxy does, so the request is priced.
func TestEnvelope_CachesplitIsReportedAndPriced(t *testing.T) {
	stable := strings.Repeat("You are a careful assistant. ", 60)
	volatile := "Today is 2026-11-17. cwd: /tmp/x"
	client := body(smallTool, `[{"type":"text","text":"`+stable+volatile+`","cache_control":{"type":"ephemeral"}}]`)
	forwarded := body(smallTool, `[{"type":"text","text":"`+stable+`","cache_control":{"type":"ephemeral"}},{"type":"text","text":"`+volatile+`"}]`)
	res, ops := priced(t, client, forwarded)
	if len(ops) != 1 || ops[0].Operator != "context-guru.cachesplit" || !strings.HasPrefix(ops[0].ChangeID, "cg:cachesplit:") ||
		ops[0].At != (accountant.Position{Section: accountant.SectionSystem, Index: 0}) {
		t.Fatalf("ops = %+v, want one context-guru.cachesplit op on system[0]", ops)
	}
	noSumFailure(t, res)
}

// toolfilter drops declarations and reports no op: one removal op covers them.
func TestEnvelope_ToolfilterIsReportedAndPriced(t *testing.T) {
	system := `"be brief"`
	client := body(bigTool+`,`+smallTool, system)
	forwarded := body(smallTool, system)
	res, ops := priced(t, client, forwarded)
	if len(ops) != 1 || ops[0].Operator != "context-guru.toolfilter" || !strings.HasPrefix(ops[0].ChangeID, "cg:toolfilter:decl-removed:") ||
		ops[0].SizeBefore.Bytes != len(bigTool) || ops[0].SizeAfter.Bytes != 0 {
		t.Fatalf("ops = %+v, want one context-guru.toolfilter removal of Big", ops)
	}
	noSumFailure(t, res)
	var saved bool
	for _, op := range res.Ops {
		if op.Operator == "context-guru.toolfilter" && !op.Saved.IsZero() {
			saved = true
		}
	}
	if !saved {
		t.Errorf("the removal saved nothing: %+v", res.Ops)
	}
}

// A request on which only messages changed has no envelope op: the engine's own ops cover it.
func TestEnvelope_MessagesOnlyChangeReportsNoEnvelopeOp(t *testing.T) {
	client := body(smallTool, `"be brief"`)
	forwarded := []byte(strings.Replace(string(client), " lorem ipsum dolor", "", 50))
	if got := envelopeOps(client, forwarded); len(got) != 0 {
		t.Errorf("envelopeOps = %+v, want none", got)
	}
	// Nor for a body with no conversation, which the accountant does not record.
	if got := envelopeOps([]byte(`{"tools":[`+bigTool+`]}`), []byte(`{"tools":[]}`)); got != nil {
		t.Errorf("no messages: envelopeOps = %+v, want nil", got)
	}
}

// A declaration rewritten in place is a toolschema op at its input index; an added one is a rewrite
// from nothing at its output index, owned by expand when it is expand's tool.
func TestEnvelope_RewrittenAndAddedDeclarations(t *testing.T) {
	short := `{"name":"Big","description":"x","input_schema":{"type":"object"}}`
	exp := `{"name":"context_guru_expand","description":"expand","input_schema":{"type":"object"}}`
	ops := envelopeOps(body(bigTool, `"s"`), body(short+`,`+exp, `"s"`))
	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want a rewrite and an addition", ops)
	}
	if ops[0].Operator != "context-guru.toolschema" || ops[0].SizeBefore.Bytes != len(bigTool) || ops[0].SizeAfter.Bytes != len(short) {
		t.Errorf("rewrite = %+v", ops[0])
	}
	if ops[1].Operator != "context-guru.expand" || ops[1].SizeBefore.Bytes != 0 || ops[1].SizeAfter.Bytes != len(exp) || ops[1].At.Index != 1 {
		t.Errorf("addition = %+v", ops[1])
	}
}
