package accounting

import (
	"bytes"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting/facts"
	"github.com/rossoctl/cortex/core/accounting/wire"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/tidwall/gjson"
)

// computeWait bounds how long Compute waits for a request's record to be ended. The work is a
// parse and two hashes, normally a few milliseconds; past this the request is recorded with no
// saving rather than held (global constraint: accounting never delays past 2s).
const computeWait = 2 * time.Second

// pendingKey is the pctx state key RequestEnd stores a request's handle under, for Compute.
const pendingKey = "accounting.pending"

// threadBillingCap and threadBillingTrimTo mirror accountant's own thread bound: the library's
// Trackers (internal/engine/state.go:47-114, maxThreads = 10000) drops the threads seen longest
// ago, down to maxThreads - maxThreads/10, once more than maxThreads are held. Host.threadBilling
// is a second, independent map keyed the same way (session + thread), so it is bounded the same
// way rather than growing for the life of the process.
const (
	threadBillingCap    = 10000
	threadBillingTrimTo = threadBillingCap - threadBillingCap/10 // 9000
)

// billingKey is the pctx state key RequestStart stores the client's billing mode under, for
// RequestEnd to read. Only the resulting mode is kept — never the header value it came from
// (accountant R2, and the global constraint that accounting never logs or stores one).
const billingKey = "accounting.billing"

// Options configure a Host.
type Options struct {
	// Resolver is the process rate table. Savings prices every Result with it (USD, TierUSD).
	Resolver pricing.Resolver
	// SnapshotPath is where the Tracker state is saved every 60s and on Close, and restored
	// from at New. Empty means no snapshot: in a cluster a pod restart starts fresh (design §5.4).
	SnapshotPath string
	// CostRecorders receive the cost record of spend the host makes outside a request (the
	// keep-alive ping, a later task). The ledger writer and the usage aggregator, the same
	// Recorders the session store feeds.
	CostRecorders []session.Recorder
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// SubscriptionWeights value a saving on a subscription login in usage-limit units
	// (accounting.subscription_weights). Nil means config.DefaultSubscriptionWeights.
	SubscriptionWeights *config.SubscriptionWeights
}

// Host is Cortex's accountant host: one per process, shared by every pipeline a reload builds,
// so the Tracker state survives a reload (Review Focus 5). It implements
// pipeline.RequestAccountant.
//
// It holds nothing per request beyond the handle it leaves on the request's own pctx, and
// nothing per thread beyond what accountant.Trackers keeps (the library bounds that to 10,000
// threads) plus one BillingMode per (session, thread) in threadBilling, for keep-alive pings
// (design §5.7) — threadBilling is bounded the same way (threadBillingCap/threadBillingTrimTo).
type Host struct {
	trackers *accountant.Trackers
	resolver pricing.Resolver
	now      func() time.Time
	weights  config.SubscriptionWeights

	snapshotPath string
	stop         chan struct{}
	saver        sync.WaitGroup
	closeOnce    sync.Once
	closeErr     error

	mu            sync.Mutex
	recorders     []session.Recorder
	threadBilling map[threadBillingKey]threadBillingEntry
	// pingFacts holds the latest keep-alive ping's facts per (session, thread), until the
	// thread's next priced request takes them (ping.go). Bounded by pingFactsCap.
	pingFacts map[threadBillingKey]pingEntry

	// Test hooks. beforeEnd runs first on the record's goroutine, computeHook just before the
	// library's Compute, onEnd with the ops the record was given, metaHook with the
	// RequestMeta RequestEnd built (synchronously, before the record's goroutine starts),
	// factsHook with the EntryFacts the record is ended with, usageHook with the
	// accountant.Usage Compute built from pctx.Extensions.Inference, just before it is
	// handed to the library (added for task 7's CacheWrite1h plumbing, 2026-11-17
	// accountant-integration — Compute's own return carries no usage, only Ops/Checks/
	// Unpriced, so a test asserting what was BUILT needs its own hook). Nil outside tests.
	beforeEnd   func()
	computeHook func()
	onEnd       func(ops []accountant.Operation)
	metaHook    func(meta accountant.RequestMeta)
	factsHook   func(f accountant.EntryFacts)
	usageHook   func(u accountant.Usage)
}

// threadBillingKey keys Host.threadBilling: a thread id is only unique within its session.
type threadBillingKey struct {
	session string
	thread  string
}

// threadBillingEntry is one Host.threadBilling value: the thread's last-seen billing mode, and
// when it was recorded, so trimThreadBillingLocked can drop the oldest entries the way
// accountant.Trackers.trim drops its oldest threads.
type threadBillingEntry struct {
	mode     accountant.BillingMode
	lastSeen time.Time
}

// New builds a Host. With a SnapshotPath it restores the saved Tracker state, when there is a
// usable one, and starts saving it every 60s.
func New(o Options) (*Host, error) {
	h := &Host{
		trackers:     accountant.NewTrackers(),
		resolver:     o.Resolver,
		now:          o.Now,
		snapshotPath: o.SnapshotPath,
		recorders:    append([]session.Recorder(nil), o.CostRecorders...),
		stop:         make(chan struct{}),
		weights:      config.DefaultSubscriptionWeights(),
	}
	if o.SubscriptionWeights != nil {
		h.weights = *o.SubscriptionWeights
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.snapshotPath != "" {
		h.restore()
		h.saver.Add(1)
		go h.saveLoop()
	}
	return h, nil
}

// AddCostRecorder adds a Recorder after New. cmd/cortex needs it: the Host must exist before the
// first pipeline build, and the ledger and the usage aggregator are opened after it.
func (h *Host) AddCostRecorder(r session.Recorder) {
	if h == nil || r == nil {
		return
	}
	h.mu.Lock()
	h.recorders = append(h.recorders, r)
	h.mu.Unlock()
}

// Close stops the snapshot saver and writes a final snapshot. Safe to call more than once.
func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() {
		close(h.stop)
		h.saver.Wait()
		if h.snapshotPath != "" {
			h.closeErr = h.save()
		}
	})
	return h.closeErr
}

// pending is one request's record, from RequestEnd to Compute. The goroutine writes req,
// serverHeld and failed, then closes done; Compute reads them only after done is closed. The
// latch fields are Compute's own, on the request goroutine.
type pending struct {
	done       chan struct{}
	req        *accountant.Request
	serverHeld bool
	// ping is the (session, thread) whose ping facts the record was ended with, and pingWindow
	// their window; Compute drops them once the request is priced. pinged is false when there
	// were none.
	ping       threadBillingKey
	pingWindow string
	pinged     bool

	computed bool
	result   accountant.Result
	ok       bool
}

// RequestStart implements pipeline.RequestAccountant. It reads the client's upstream credential
// header — pctx.Headers, as the listener filled it, before any plugin can rewrite it — and
// stores the billing mode it implies for RequestEnd. token-exchange replaces Authorization on
// outbound requests, so this has to run here and not in RequestEnd (design §5.7).
//
// accountant.BillingFrom reads only the credential's type and returns no part of the credential
// itself (accountant R2); only the resulting mode is stored, never the header value.
func (h *Host) RequestStart(pctx *pipeline.Context) {
	if h == nil || pctx == nil {
		return
	}
	billing := accountant.BillingFrom(pctx.Headers)
	pipeline.SetState(pctx, billingKey, &billing)
}

// RequestEnd implements pipeline.RequestAccountant. It takes what the record needs from pctx and
// starts the record on a goroutine of its own: the parse, BeginRequest, each op and EndRequest
// never run on the request goroutine (global constraint).
//
// Traffic that is not an inference request, or that has no session to key a thread on, starts
// nothing and logs nothing (Review Focus 2).
func (h *Host) RequestEnd(pctx *pipeline.Context) {
	if h == nil || pctx == nil || pctx.SessionID == "" {
		return
	}
	format, ok := wire.FormatOf(pctx.Path)
	if !ok {
		return
	}
	client := pctx.ClientBody()
	if len(client) == 0 {
		return
	}
	forwarded := pctx.Body
	model := ""
	ops := pctx.ReportedOps()
	if inf := pctx.Extensions.Inference; inf != nil {
		model = inf.Model
		if op, ok := modelChangeOp(pctx, inf.RequestedModel, inf.Model); ok {
			ops = append(ops, op)
		}
	}
	host := pctx.Host
	// The destination after any redirect: that is the server that bills.
	if _, redirected, ok := pctx.RedirectTarget(); ok {
		host = redirected
	}
	billing := accountant.Billing{Mode: accountant.BillingUnknown}
	if b := pipeline.GetState[accountant.Billing](pctx, billingKey); b != nil {
		billing = *b
	}
	meta := accountant.RequestMeta{
		RequestID: pctx.RequestID(),
		Session:   pctx.SessionID,
		Model:     model,
		At:        h.now(),
		Billing:   billing,
	}
	thread := ThreadOf(pctx)
	h.setThreadBilling(pctx.SessionID, thread.ID, billing.Mode)
	if h.metaHook != nil {
		h.metaHook(meta)
	}
	p := &pending{done: make(chan struct{})}
	pipeline.SetState(pctx, pendingKey, p)
	go h.end(p, format, meta, client, forwarded, ops, thread, facts.Classify(host, pctx.Path))
}

// ThreadBilling returns the billing mode of the given thread's last request, for a keep-alive
// ping on that thread: a ping uses the same credential as the request it pings for (design
// §5.7). accountant.BillingUnknown if the thread has made no request yet.
func (h *Host) ThreadBilling(session, thread string) accountant.Billing {
	if h == nil {
		return accountant.Billing{Mode: accountant.BillingUnknown}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.threadBilling[threadBillingKey{session: session, thread: thread}]
	if !ok {
		return accountant.Billing{Mode: accountant.BillingUnknown}
	}
	return accountant.Billing{Mode: e.mode}
}

// setThreadBilling records mode as thread's last-seen billing mode, and trims the map if that
// pushed it over threadBillingCap. Runs on the request goroutine, inside RequestEnd.
func (h *Host) setThreadBilling(session, thread string, mode accountant.BillingMode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.threadBilling == nil {
		h.threadBilling = map[threadBillingKey]threadBillingEntry{}
	}
	h.threadBilling[threadBillingKey{session: session, thread: thread}] = threadBillingEntry{
		mode:     mode,
		lastSeen: h.now(),
	}
	h.trimThreadBillingLocked()
}

// trimThreadBillingLocked drops the (session, thread) pairs seen longest ago, down to
// threadBillingTrimTo, once threadBilling holds more than threadBillingCap — the same amortized
// trim accountant.Trackers.trim uses for its own thread bound (library
// internal/engine/state.go:84-114): the sort runs only on the call that crosses the cap, not on
// every insert. Callers hold h.mu.
func (h *Host) trimThreadBillingLocked() {
	if len(h.threadBilling) <= threadBillingCap {
		return
	}
	type aged struct {
		key threadBillingKey
		at  time.Time
	}
	all := make([]aged, 0, len(h.threadBilling))
	for k, e := range h.threadBilling {
		all = append(all, aged{k, e.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(h.threadBilling)-threadBillingTrimTo] {
		delete(h.threadBilling, a.key)
	}
}

// end opens and ends the record. A panic here, on a hostile body or a library bug, costs this
// request's figure and nothing else: the request is already on its way, and an unrecovered panic
// on this goroutine would end the process.
func (h *Host) end(p *pending, format wire.Format, meta accountant.RequestMeta, client, forwarded []byte,
	ops []accountant.Operation, thread accountant.Thread, provider facts.Provider) {
	defer close(p.done)
	defer func() {
		if r := recover(); r != nil {
			p.req = nil
			slog.Error("accounting: the record panicked; the request is recorded with no saving", "panic", r)
		}
	}()
	if h.beforeEnd != nil {
		h.beforeEnd()
	}
	// The provider holds part of the prompt, so this body is not the prefix it bills: no record
	// at all, or the library would read the missing history as a client edit (W6.1, #899).
	if format == wire.FormatResponses && responsesServerHeldHistory(client) {
		p.serverHeld = true
		return
	}
	clientBody, clientBPs, err := wire.Parse(format, client)
	if err != nil {
		slog.Debug("accounting: client body not parsed; no record", "error", err)
		return
	}
	fwdBody, fwdBPs := clientBody, clientBPs
	// A request no plugin changed is not hashed twice: a large body costs milliseconds.
	if !bytes.Equal(client, forwarded) {
		if fwdBody, fwdBPs, err = wire.Parse(format, forwarded); err != nil {
			slog.Debug("accounting: forwarded body not parsed; no record", "error", err)
			return
		}
	}
	req := h.trackers.BeginRequest(meta, clientBody, clientBPs)
	for _, op := range ops {
		req.Add(op)
	}
	// The cache facts come from the bytes sent: the marks there are the ones the provider reads.
	f := facts.For(provider, meta.Model, wire.AllMarksAsk1h(forwarded))
	// The thread's last ping, if one was sent since its previous request (design §5.8). Kept
	// until Compute prices this request, so a request that books nothing leaves them.
	if pf, ok := h.peekPing(meta.Session, thread.ID); ok {
		pf.Apply(&f)
		p.ping = threadBillingKey{session: meta.Session, thread: thread.ID}
		p.pingWindow, p.pinged = pf.PingWindow, true
	}
	if h.factsHook != nil {
		h.factsHook(f)
	}
	req.EndRequest(thread, fwdBody, fwdBPs, f)
	if h.onEnd != nil {
		h.onEnd(ops)
	}
	p.req = req
}

// Compute prices pctx's request with the usage the response billed, once: a second call returns
// the first call's answer, so a repeated settle cannot price twice. ok is false when there is no
// record (non-inference traffic, a refused request), when a W6.1 rule skips it, when the record
// is not ended within computeWait, and when the library panics. All of those record the request
// with no saving: accounting fails open.
func (h *Host) Compute(pctx *pipeline.Context) (accountant.Result, bool) {
	if h == nil || pctx == nil {
		return accountant.Result{}, false
	}
	p := pipeline.GetState[pending](pctx, pendingKey)
	if p == nil {
		return accountant.Result{}, false
	}
	if p.computed {
		return p.result, p.ok
	}
	p.computed = true

	in := SkipInput{Status: pctx.StatusCode, Rounds: 1 + len(pctx.ResendReasons()), SessionID: pctx.SessionID}
	if why := SkipReason(in); why != "" {
		h.release(pctx, why)
		return accountant.Result{}, false
	}
	timer := time.NewTimer(computeWait)
	select {
	case <-p.done:
		timer.Stop()
	case <-timer.C:
		slog.Warn("accounting: the record was not ended in time; the request is recorded with no saving",
			"wait", computeWait)
		return accountant.Result{}, false
	}
	in.ServerHeldHistory = p.serverHeld
	if why := SkipReason(in); why != "" {
		h.release(pctx, why)
		return accountant.Result{}, false
	}
	inf := pctx.Extensions.Inference
	if p.req == nil || inf == nil {
		h.release(pctx, "no_record")
		return accountant.Result{}, false
	}
	// CacheWrite1h is the part of CacheWrite billed at the 1-hour rate, never an
	// addition to it (accountant.Usage's own doc). Task 7 (2026-11-17
	// accountant-integration): the 1h cache-write tier.
	usage := accountant.Usage{
		Input:        inf.InputTokens,
		CacheRead:    inf.CacheReadTokens,
		CacheWrite:   inf.CacheWriteTokens,
		CacheWrite1h: inf.CacheWrite1hTokens,
	}
	if h.usageHook != nil {
		h.usageHook(usage)
	}
	p.result, p.ok = h.computeSafe(p.req, usage)
	p.req = nil
	if p.ok && p.pinged {
		h.dropPing(p.ping.session, p.ping.thread, p.pingWindow)
	}
	return p.result, p.ok
}

// release drops a request's handle and its library record: Compute will not run for it.
func (h *Host) release(pctx *pipeline.Context, why string) {
	// Only the handle goes: the record's goroutine may still be running, and it ends on its own.
	// Once it has, nothing references the library record.
	delete(pctx.Extensions.Custom, pendingKey)
	slog.Debug("accounting: no figure for this request", "reason", why)
}

// computeSafe is the library's Compute under a recover, as the library asks a host to wrap it.
func (h *Host) computeSafe(req *accountant.Request, u accountant.Usage) (res accountant.Result, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accounting: Compute panicked; the request is recorded with no saving", "panic", r)
			res, ok = accountant.Result{}, false
		}
	}()
	if h.computeHook != nil {
		h.computeHook()
	}
	return req.Compute(u), true
}

// modelChangeOp is the setting op for a plugin that changed the model (SetRequestModel): the
// change is not a body edit the library can see by comparing positions, and it starts at the
// first position, because a model change invalidates the whole cached prefix (cached-prefix.md).
// The operator is the plugin that made the last applied change, from its modify/model_rewritten
// record.
func modelChangeOp(pctx *pipeline.Context, from, to string) (accountant.Operation, bool) {
	if from == "" || to == "" || from == to {
		return accountant.Operation{}, false
	}
	operator := "cortex.model"
	if invs := pctx.Extensions.Invocations; invs != nil {
		for _, list := range [][]pipeline.Invocation{invs.Inbound, invs.Outbound} {
			for _, inv := range list {
				if inv.Reason == "model_rewritten" && !inv.Shadow && inv.Plugin != "" {
					operator = inv.Plugin
				}
			}
		}
	}
	return accountant.Operation{
		Kind:     accountant.KindSetting,
		Operator: operator,
		ChangeID: "model:" + from + "->" + to,
		Settings: map[string]accountant.SettingChange{"model": {
			From: &from,
			To:   &to,
			At:   &accountant.Position{Section: accountant.SectionTools, Index: 0},
		}},
	}, true
}

// responsesServerHeldHistory says whether a Responses body leaves part of its prompt on the
// provider's side: previous_response_id (earlier turns of a response chain), conversation (a
// stored conversation, by id string or object), or an input item of type item_reference. Ported
// from the context-guru proxy's rule of the same name (proxy/accountant_wire.go).
func responsesServerHeldHistory(body []byte) bool {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return true
	}
	switch c := gjson.GetBytes(body, "conversation"); {
	case c.Type == gjson.String && c.String() != "", c.IsObject():
		return true
	}
	held := false
	gjson.GetBytes(body, "input").ForEach(func(_, it gjson.Result) bool {
		held = it.Get("type").String() == "item_reference"
		return !held
	})
	return held
}
