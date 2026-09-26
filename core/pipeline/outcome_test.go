package pipeline

import "testing"

// TestOutcomeFromContext_ALateRefusalIsNotADenial covers the pipeline half of a defect found in
// ext_proc's teardown flush: a plugin refusing a response that has already gone downstream.
//
// The refusal is real and stays on the record — a gate that would have blocked a response after it
// shipped is exactly what a rollout wants to see — but it cannot be the request's OUTCOME, because
// the request was answered. Reported as a denial, it tells every Finisher, audit row and dashboard
// that a 200 was blocked.
//
// BOTH DENY PATHS ARE COVERED, because they are two answers to one question and either one alone
// leaves the other lying: the rejecting-plugin state that RunResponse sets, and the deny Invocation
// that lastDenyingPlugin walks. The subtests below drive them separately.
func TestOutcomeFromContext_ALateRefusalIsNotADenial(t *testing.T) {
	t.Run("a recorded deny invocation", func(t *testing.T) {
		pctx := &Context{StatusCode: 200}
		pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
		pctx.MarkResponseDelivered()
		pctx.Record(Invocation{Action: ActionDeny, Reason: "too late"})

		out := OutcomeFromContext(pctx)
		if out.FinalAction == OutcomeDeny {
			t.Errorf("FinalAction = %v on a delivered response (status %d): a refusal recorded after delivery is not a denial",
				out.FinalAction, out.StatusCode)
		}
		// MARKED, NOT DROPPED. The distinction only exists if the row survives to carry it.
		invs := pctx.Extensions.Invocations
		if invs == nil || len(invs.Inbound)+len(invs.Outbound) != 1 {
			t.Fatalf("invocations = %+v, want exactly one", invs)
		}
		got := append(append([]Invocation{}, invs.Inbound...), invs.Outbound...)[0]
		if !got.Late {
			t.Errorf("Late = false on %+v: without the marker nothing downstream can tell a refusal that took effect from one that could not", got)
		}
	})

	t.Run("the rejecting-plugin state", func(t *testing.T) {
		pctx := &Context{StatusCode: 200}
		pctx.MarkResponseDelivered()
		pctx.setRejectingPlugin("gate")

		if out := OutcomeFromContext(pctx); out.FinalAction == OutcomeDeny {
			t.Errorf("FinalAction = %v via the rejecting-plugin branch: the two deny paths have to agree, or the same request has two outcomes",
				out.FinalAction)
		}
		// The evidence is kept: what changed is what it MEANS, not whether it happened.
		if pctx.RejectingPlugin() != "gate" {
			t.Errorf("RejectingPlugin = %q, want \"gate\"", pctx.RejectingPlugin())
		}
	})

	t.Run("the control: before delivery it IS a denial", func(t *testing.T) {
		pctx := &Context{StatusCode: 403}
		pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
		pctx.Record(Invocation{Action: ActionDeny, Reason: "blocked"})
		pctx.setRejectingPlugin("gate")

		out := OutcomeFromContext(pctx)
		if out.FinalAction != OutcomeDeny || out.DenyingPlugin != "gate" {
			t.Errorf("outcome = %+v, want a denial by gate: an ordinary refusal must still read as one, or this fix has broken enforcement reporting",
				out)
		}
	})
}

// TestOutcomeFromContext_ARefusalBeforeDeliveryStaysADenial is the other side of the same rule, and
// the reason rejectedAfterDelivery is a SNAPSHOT taken when the refusal is recorded rather than a read
// of responseDelivered at outcome time.
//
// The obvious simplification — asking "is the response delivered?" when the outcome is derived —
// inverts this fix for a real case: a gate that refuses BEFORE anything is sent, whose error page is
// then delivered. That is a delivered response with a genuine block, and mutation-tested, the
// simplification reports it as ALLOWED:
//
//	bare Deny pre-delivery, then delivered   FinalAction=allow DenyingPlugin=""   (mutant)
//	bare Deny pre-delivery, then delivered   FinalAction=deny  DenyingPlugin=gate (this code)
//
// The whole core module passes under that mutant — 59 packages, exit 0 — so nothing else would
// notice a real block reported as an allow.
//
// THE BARE-Deny SHAPE IS THE ONE THAT DISCRIMINATES, which is worth knowing before trusting the
// second subtest below: with an Invocation on record the mutant still reports deny, because
// lastDenyingPlugin finds the row and it is not Late. Both are pinned anyway — they are both real
// properties, and a future change could break either — but only the first fails under this mutation.
func TestOutcomeFromContext_ARefusalBeforeDeliveryStaysADenial(t *testing.T) {
	t.Run("a bare Deny, then the refusal is delivered", func(t *testing.T) {
		pctx := &Context{StatusCode: 403}
		pctx.setRejectingPlugin("gate")
		// An error page IS a delivered response, so a listener says so afterwards.
		pctx.MarkResponseDelivered()

		out := OutcomeFromContext(pctx)
		if out.FinalAction != OutcomeDeny || out.DenyingPlugin != "gate" {
			t.Errorf("outcome = %+v, want a denial by gate: this refusal took effect — the client got the 403 — and reporting it as an allow is the inverse of the bug this file is about",
				out)
		}
	})

	t.Run("a recorded deny, then the refusal is delivered", func(t *testing.T) {
		pctx := &Context{StatusCode: 403}
		pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
		pctx.Record(Invocation{Action: ActionDeny, Reason: "blocked"})
		pctx.setRejectingPlugin("gate")
		pctx.MarkResponseDelivered()

		out := OutcomeFromContext(pctx)
		if out.FinalAction != OutcomeDeny {
			t.Errorf("outcome = %+v, want a denial", out)
		}
		// AND THE ROW IS NOT MARKED LATE, because it was recorded before delivery. Late is about
		// WHEN the decision was taken, not about the state the request ended in.
		invs := append(append([]Invocation{}, pctx.Extensions.Invocations.Inbound...), pctx.Extensions.Invocations.Outbound...)
		if len(invs) != 1 {
			t.Fatalf("invocations = %+v, want exactly one", invs)
		}
		if invs[0].Late {
			t.Error("Late = true on a refusal recorded BEFORE delivery: the mark would then say the decision did not take effect, when it did")
		}
	})
}

// TestMarkResponseDelivered_IsIdempotentAndOneWay pins the two properties its doc claims, neither of
// which was asserted.
//
// One-way matters because a listener may call it once and then run several dispatches: if anything
// could clear it, the refusals recorded after those dispatches would go back to renaming the request.
func TestMarkResponseDelivered_IsIdempotentAndOneWay(t *testing.T) {
	pctx := &Context{StatusCode: 200}
	if pctx.ResponseDelivered() {
		t.Fatal("ResponseDelivered() = true before anything said so")
	}
	pctx.MarkResponseDelivered()
	pctx.MarkResponseDelivered()
	if !pctx.ResponseDelivered() {
		t.Error("ResponseDelivered() = false after two calls: idempotent means the second is a no-op, not an undo")
	}
	// A refusal recorded now is still Late after the repeat calls.
	pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
	pctx.Record(Invocation{Action: ActionDeny, Reason: "too late"})
	invs := append(append([]Invocation{}, pctx.Extensions.Invocations.Inbound...), pctx.Extensions.Invocations.Outbound...)
	if len(invs) != 1 || !invs[0].Late {
		t.Errorf("invocations = %+v, want one marked Late", invs)
	}
	if out := OutcomeFromContext(pctx); out.FinalAction == OutcomeDeny {
		t.Errorf("outcome = %+v: a late refusal cannot rename a delivered request", out)
	}
}
