package cpex

import (
	"net/http"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A policy step such as require_approval(...) suspends a call until a
// human answers. CPEX reports the suspension as a violation with one of
// these codes, and the client is expected to retry, echoing the
// elicitation id in X-Policy-Elicitation-Id. The JSON-RPC codes are the
// ones CPEX's agent SDK keys its pause/resume loop on.
const (
	cpexElicitationPending  = "elicitation.pending"
	cpexElicitationApproved = "elicitation.approved" // a peek that found the approval granted

	rpcElicitationPending  = -32120
	rpcElicitationApproved = -32121
)

// Elicitation is a call CPEX suspended pending a human's answer.
type Elicitation struct {
	// Code is the violation code: elicitation.pending or elicitation.approved.
	Code string
	// ID is what the client echoes on retry. It goes to the client only,
	// never to the session record.
	ID        string
	Approver  string
	ExpiresAt string
}

// elicitationFromViolation returns the suspension a violation reports, or
// nil when the violation is an ordinary deny. Values that do not look
// like identifiers are dropped rather than reflected to the client.
func elicitationFromViolation(code string, details map[string]any) *Elicitation {
	if code != cpexElicitationPending && code != cpexElicitationApproved {
		return nil
	}
	str := func(k string, max int) string {
		s, _ := details[k].(string)
		if len(s) == 0 || len(s) > max {
			return ""
		}
		for i := 0; i < len(s); i++ {
			if c := s[i]; c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
				return ""
			}
		}
		return s
	}
	return &Elicitation{
		Code:      code,
		ID:        str("elicitation_id", 4096),
		Approver:  str("approver", 256),
		ExpiresAt: str("expires_at", 64),
	}
}

// suspend rejects a call CPEX suspended. An MCP client gets a JSON-RPC
// error frame with the retry code and the elicitation id; anything else
// gets a 403 carrying the same. The message is static.
func suspend(pctx *pipeline.Context, e *Elicitation, details map[string]string) pipeline.Action {
	code, rpc, msg := "cpex.elicitation_pending", rpcElicitationPending,
		"approval pending: retry with the elicitation id"
	if e.Code == cpexElicitationApproved {
		code, rpc, msg = "cpex.elicitation_approved", rpcElicitationApproved,
			"approved: re-send without the peek header to apply"
	}

	recorded := make(map[string]string, len(details)+2)
	for k, v := range details {
		recorded[k] = v
	}
	client := map[string]any{}
	if e.ID != "" {
		client["elicitation_id"] = e.ID
	}
	if e.Approver != "" {
		recorded["approver"] = e.Approver
		client["approver"] = e.Approver
	}
	if e.ExpiresAt != "" {
		recorded["expires_at"] = e.ExpiresAt
		client["expires_at"] = e.ExpiresAt
	}
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: code, Details: recorded})
	return pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{
		Code:    code,
		Reason:  msg,
		Status:  http.StatusForbidden,
		Details: client,
		RPCCode: rpc,
	}}
}
