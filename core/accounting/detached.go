package accounting

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// detachedRequest is what LateOps keeps of a request, for the model calls that finish after it
// was accounted. It is read on the request goroutine and never changes after that, so a late
// call can use it from any goroutine.
type detachedRequest struct {
	requestID string
	session   string
	thread    string
	host      string
	billing   accountant.Billing
	client    *model.EventClient
	// seq numbers the late calls of this request, for their event RequestIDs.
	seq *atomic.Int64
}

// LateOps implements pipeline.LateOpsAccountant. It returns the handler for pctx's context
// operations that are reported after the request was accounted: a model call that an engine
// detached from the request, such as a background summarizer (cortex#2140, wiring W4.8 B).
//
// The handler keeps each spend that has a Call, and books it on its own goroutine as a
// standalone spend (bookDetached). It keeps no other kind: a late rewrite cannot change bytes
// that were already sent. A request with no session gets no handler, as RequestEnd starts no
// record for it.
func (h *Host) LateOps(pctx *pipeline.Context) func([]accountant.Operation) int {
	if h == nil || pctx == nil || pctx.SessionID == "" {
		return nil
	}
	host := pctx.Host
	if _, redirected, ok := pctx.RedirectTarget(); ok {
		host = redirected
	}
	billing := accountant.Billing{Mode: accountant.BillingUnknown}
	if b := pipeline.GetState[accountant.Billing](pctx, billingKey); b != nil {
		billing = *b
	}
	d := detachedRequest{
		requestID: pctx.RequestID(), session: pctx.SessionID, thread: ThreadOf(pctx).ID,
		host: host, billing: billing, client: pctx.ClientInfo(), seq: new(atomic.Int64),
	}
	return func(ops []accountant.Operation) int { return h.lateOps(d, ops) }
}

// lateOps is the LateOps handler: it keeps the spends and starts their booking.
func (h *Host) lateOps(d detachedRequest, ops []accountant.Operation) int {
	spends := make([]accountant.Operation, 0, len(ops))
	for _, op := range ops {
		if op.Kind == accountant.KindSpend && op.Call != nil {
			spends = append(spends, op)
			continue
		}
		slog.Debug("accounting: a context operation other than a spend came after the request was accounted; dropped",
			"operator", op.Operator, "kind", op.Kind)
	}
	if len(spends) == 0 {
		return 0
	}
	if !h.goObserve(func() { h.bookDetached(d, spends) }) {
		return 0
	}
	return len(spends)
}

// DetachedSpend is one booked detached model call, as a detached-spend listener receives it.
// Figures holds only the PluginSpend* fields: the call's cost by basis.
type DetachedSpend struct {
	Session, Thread, Operator, Model, Host string
	At                                     time.Time
	Figures                                event.Figures
}

// DetachedSpendListener receives every detached model call Host books. It is called on the
// booking goroutine, with no Host lock held; it must not block. A panic in it is recovered.
type DetachedSpendListener func(DetachedSpend)

// AddDetachedSpendListener registers fn for every detached model call booked from now on, and
// returns the function that removes it. cmd/cortex's session store uses it: a detached call is
// not an event, so it reaches the store no other way.
func (h *Host) AddDetachedSpendListener(fn DetachedSpendListener) (remove func()) {
	if h == nil || fn == nil {
		return func() {}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.detachedListeners == nil {
		h.detachedListeners = map[uint64]DetachedSpendListener{}
	}
	h.pingListenerSeq++
	id := h.pingListenerSeq
	h.detachedListeners[id] = fn
	return func() {
		h.mu.Lock()
		delete(h.detachedListeners, id)
		h.mu.Unlock()
	}
}

// bookDetached prices and records detached spends, by the rules of an in-request spend:
//
//   - accountant.ComputeStandalone checks each call. It reads and changes no ThreadState.
//   - spendRecord prices each call at its own model, host and prompt size (CallUSDCurrency), in
//     the basis of the request's billing mode. A dry run is Projected, a refused call Refused,
//     and neither enters a total.
//   - Each call is one cost event with Purpose event.PurposeDetachedSpend. The ledger and
//     /v1/usage book its Spends as plugin spend, and count no request for it.
//   - The metrics sink gets the Result (Sink.ObserveDetached), and the listeners get the figures.
//
// It runs on a goroutine of its own (goObserve). A panic costs this call's figure and nothing
// else.
func (h *Host) bookDetached(d detachedRequest, ops []accountant.Operation) {
	at := h.now()
	res := computeStandalone(accountant.RequestOps{
		Version: 1, RequestID: d.requestID, Session: d.session, Thread: accountant.Thread{ID: d.thread},
		At: at, Billing: d.billing, Ops: ops,
	})
	refused := map[string]bool{}
	for _, u := range res.Unpriced {
		if u.OpID != "" {
			refused[u.OpID] = true
		}
	}
	basis := BasisOf(d.billing.Mode)
	spend := func(c *accountant.ModelCall, at string) (float64, bool, string) {
		return CallUSDCurrency(h.resolver, c, at)
	}
	h.mu.Lock()
	recs := append([]session.Recorder(nil), h.recorders...)
	fns := make([]DetachedSpendListener, 0, len(h.detachedListeners))
	for _, fn := range h.detachedListeners {
		fns = append(fns, fn)
	}
	h.mu.Unlock()
	for _, op := range res.Ops {
		if op.Call == nil {
			continue
		}
		r := spendRecord(op, spend, d.host, basis, h.weights)
		r.Projected, r.Refused, r.Thread = op.DryRun, refused[op.OpID], d.thread
		rec := event.Event{Purpose: event.PurposeDetachedSpend, Thread: d.thread, Currency: r.Currency,
			Spends: []event.SpendRecord{r}}
		h.recordDetached(recs, d, r, rec, at)
		figs := rec.Figures()
		for _, fn := range fns {
			func() {
				defer func() {
					if p := recover(); p != nil {
						slog.Error("accounting: detached-spend listener panicked", "panic", p)
					}
				}()
				fn(DetachedSpend{Session: d.session, Thread: d.thread, Operator: op.Operator, Model: r.Model,
					Host: r.Host, At: at, Figures: figs})
			}()
		}
	}
	h.sink.ObserveDetached(accountant.RequestMeta{RequestID: d.requestID, Session: d.session, At: at,
		Billing: d.billing}, res, d.host)
}

// recordDetached writes one detached call's cost event to every cost recorder.
func (h *Host) recordDetached(recs []session.Recorder, d detachedRequest, r event.SpendRecord, rec event.Event, at time.Time) {
	if len(recs) == 0 {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		slog.Debug("accounting: detached spend record not encoded", "error", err)
		return
	}
	id := "detached:" + d.requestID + ":" + strconv.FormatInt(d.seq.Add(1), 10)
	for _, rc := range recs {
		// Each recorder gets its own event: a Recorder may keep the pointer.
		ev := &model.SessionEvent{
			SessionID: d.session,
			At:        at,
			Direction: model.Outbound,
			Phase:     model.SessionResponse,
			RequestID: id,
			Inference: &model.InferenceExtension{Model: r.Model},
			Plugins:   map[string]json.RawMessage{event.Key: raw, event.PluginName: raw},
			Host:      r.Host,
			// Not an error row: the call reached the provider, or it would report no spend.
			StatusCode: 200,
			Client:     d.client,
		}
		rc.Record(d.session, ev)
	}
}
