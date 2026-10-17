package cpex

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestElicitationFromViolation(t *testing.T) {
	if e := elicitationFromViolation("policy.deny", map[string]any{"elicitation_id": "x"}); e != nil {
		t.Fatalf("ordinary deny = %+v, want nil", e)
	}
	e := elicitationFromViolation(cpexElicitationPending, map[string]any{
		"elicitation_id": "eyJh.b-c_d",
		"approver":       "alice",
		"expires_at":     "2026-10-15T12:00:00Z",
		"plugin":         "manager-approver",
	})
	if e == nil || e.ID != "eyJh.b-c_d" || e.Approver != "alice" || e.ExpiresAt != "2026-10-15T12:00:00Z" {
		t.Fatalf("pending = %+v", e)
	}
	// Values that are not plain tokens are not reflected to the client.
	e = elicitationFromViolation(cpexElicitationApproved, map[string]any{
		"elicitation_id": "id with spaces",
		"approver":       42,
	})
	if e == nil || e.Code != cpexElicitationApproved || e.ID != "" || e.Approver != "" {
		t.Fatalf("approved = %+v", e)
	}
}

func TestDispatch_ElicitationPendingSuspends(t *testing.T) {
	fake := &FakeManager{Hooks: map[string]Result{
		HookToolPreInvoke: {
			Decision: DecisionDeny,
			Code:     cpexElicitationPending,
			Reason:   "awaiting approval `abc` via `manager-approver`",
			Details:  map[string]string{"tool": "adjust_compensation"},
			Elicitation: &Elicitation{
				Code: cpexElicitationPending, ID: "abc", Approver: "alice", ExpiresAt: "2026-10-15T12:00:00Z",
			},
		},
	}}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	pctx := &pipeline.Context{Direction: pipeline.Outbound}
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation == nil {
		t.Fatalf("action = %+v, want Reject", a)
	}
	v := a.Violation
	if v.Code != "cpex.elicitation_pending" || v.RPCCode != rpcElicitationPending || v.Status != http.StatusForbidden {
		t.Fatalf("violation = %+v", v)
	}
	if v.Reason != "approval pending: retry with the elicitation id" {
		t.Fatalf("reason = %q, want the static message", v.Reason)
	}
	if v.Details["elicitation_id"] != "abc" || v.Details["approver"] != "alice" {
		t.Fatalf("details = %v, want the id and approver for the client", v.Details)
	}
	inv := pctx.Extensions.Invocations.Outbound
	if len(inv) != 1 || inv[0].Action != pipeline.ActionDeny || inv[0].Reason != "cpex.elicitation_pending" {
		t.Fatalf("invocations = %+v", inv)
	}
	if _, ok := inv[0].Details["elicitation_id"]; ok {
		t.Fatalf("the session record carries the elicitation id: %v", inv[0].Details)
	}
	if inv[0].Details["approver"] != "alice" || inv[0].Details["tool"] != "adjust_compensation" {
		t.Fatalf("recorded details = %v", inv[0].Details)
	}
}

func TestDispatch_ElicitationApprovedPeek(t *testing.T) {
	fake := &FakeManager{Hooks: map[string]Result{
		HookToolPreInvoke: {
			Decision:    DecisionDeny,
			Code:        cpexElicitationApproved,
			Elicitation: &Elicitation{Code: cpexElicitationApproved, ID: "abc"},
		},
	}}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	a := p.OnRequest(context.Background(), &pipeline.Context{Direction: pipeline.Outbound})
	if a.Violation == nil || a.Violation.Code != "cpex.elicitation_approved" || a.Violation.RPCCode != rpcElicitationApproved {
		t.Fatalf("violation = %+v", a.Violation)
	}
}
