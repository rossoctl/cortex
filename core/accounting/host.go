package accounting

import (
	"bytes"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

// closeObserveWait bounds how long Close waits for the sink's in-flight observations.
const closeObserveWait = 2 * time.Second

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
	// Sink receives each computed request's Result, once, and each keep-alive ping's, for the
	// llm.savings.* counters (design §5.10). Nil means no metrics.
	Sink *Sink
}

// Host is Cortex's accountant host: one per process, shared by every pipeline a reload builds,
// so the Tracker state survives a reload (Review Focus 5). It implements
// pipeline.RequestAccountant.
//
// It holds nothing per request beyond the handle it leaves on the request's own pctx, and
// nothing per thread beyond what accountant.Trackers keeps (the library bounds that to 10,000
// threads) plus one BillingMode per (session, thread) in threadBilling, for keep-alive pings
// (design §5.7), and one last forwarded body per (session, thread) in siblings, for
// EntryFacts.SharedWarm. Both are bounded the same way (threadBillingCap/threadBillingTrimTo).
type Host struct {
	trackers *accountant.Trackers
	resolver pricing.Resolver
	now      func() time.Time
	weights  config.SubscriptionWeights
	sink     *Sink
	// observing counts the sink observations still running (observe); Close waits for them.
	observing sync.WaitGroup

	snapshotPath string
	stop         chan struct{}
	saver        sync.WaitGroup
	// dirty is set when a record changes the Tracker state (EndRequest, Compute), and cleared by
	// a periodic save. The library's Snapshot holds its tracker lock for the whole marshal,
	// which blocks every BeginRequest meanwhile, so an idle process does not take it at all.
	dirty atomic.Bool
	// snapshotTooBig is true while the state is over snapshotMaxBytes, so save WARNs once per
	// crossing. Only save touches it, and saves are serialized: the saveLoop goroutine, then
	// Close after saver.Wait.
	snapshotTooBig bool
	closeOnce      sync.Once
	closeErr       error

	mu sync.Mutex
	// closed is set under mu by Close, before it calls observing.Wait(): observe's own Add(1)
	// is also under mu, so the two can never interleave, and no Add can land after Wait has
	// started (review finding — Add concurrent with Wait on a zero-count WaitGroup is
	// documented misuse, which a keeper Ping or an in-flight observe racing shutdown could hit).
	closed        bool
	recorders     []session.Recorder
	threadBilling map[threadBillingKey]threadBillingEntry
	// pingFacts holds the latest keep-alive ping's facts per (session, thread), until the
	// thread's next priced request takes them (ping.go). Bounded by pingFactsCap.
	pingFacts map[threadBillingKey]pingEntry
	// pingListeners are the registered ping listeners (AddPingListener, AddKeyedPingListener),
	// by registration id, which grows, so removal is exact and the newest of a key is the
	// highest id.
	pingListeners map[uint64]pingListenerEntry
	// detachedListeners are the registered detached-spend listeners (AddDetachedSpendListener),
	// by registration id.
	detachedListeners map[uint64]DetachedSpendListener
	// listenerSeq gives the registration ids of both listener maps. One counter is enough:
	// an id is only compared inside its own map.
	listenerSeq uint64

	// siblings holds each (session, thread)'s last forwarded body, for EntryFacts.SharedWarm
	// (sibling.go, accountant R54). It has its own lock and is bounded like threadBilling.
	siblings siblingStore

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
	// computeWait overrides the package computeWait when positive. Tests only: a test that runs
	// hundreds of concurrent requests under -race needs more than the production bound.
	computeWait time.Duration
	onEnd       func(ops []accountant.Operation)
	metaHook    func(meta accountant.RequestMeta)
	factsHook   func(f accountant.EntryFacts)
	usageHook   func(u accountant.Usage)
	// observeHook sees the RequestMeta of each observation, on its goroutine, before the sink.
	observeHook func(meta accountant.RequestMeta)
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
//
// New cannot fail: a snapshot that cannot be read only WARNs and starts fresh, because accounting
// fails open. So it returns no error, and a caller has no "accounting could not start" branch to
// get wrong.
func New(o Options) *Host {
	h := &Host{
		trackers:     accountant.NewTrackers(),
		resolver:     o.Resolver,
		now:          o.Now,
		snapshotPath: o.SnapshotPath,
		recorders:    append([]session.Recorder(nil), o.CostRecorders...),
		stop:         make(chan struct{}),
		weights:      config.DefaultSubscriptionWeights(),
		sink:         o.Sink,
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
	return h
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
		// Set before Wait, under the same lock observe's Add(1) takes: no observe call started
		// after this point can race Wait below (see Host.closed's doc comment).
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		// The sink's observations first: they are in-memory adds, done in microseconds, and the
		// caller flushes the metrics push after Close. BOUNDED: a wedged Sink.Observe must not
		// hold process shutdown past the supervisor's stop grace (review finding). Past
		// closeObserveWait the observations still running are abandoned, and their counts are
		// not in the final push.
		observed := make(chan struct{})
		go func() { h.observing.Wait(); close(observed) }()
		select {
		case <-observed:
		case <-time.After(closeObserveWait):
			slog.Warn("accounting: observations still running at shutdown were abandoned; the final metrics push is short",
				"wait", closeObserveWait)
		}
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
// latch fields are Compute's own; computeMu guards them so a second concurrent Compute call
// (today none exists — core/pipeline.SetState/GetState document pctx as single-goroutine per
// request, and InferenceParser's own caller only ever calls Compute/Savings from the one response
// pass — but the doc on Compute itself promises "a second call returns the first call's answer"
// unconditionally) blocks on the first's full timeout-or-completion instead of racing it: both
// running computeSafe, double-counting the saving, or reading result/ok mid-write (review
// finding).
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
	// meta is the RequestMeta the record was begun with, for the sink.
	meta accountant.RequestMeta
	// firstOnThread is true when RequestEnd had no earlier request of this (session, thread),
	// for the cold_start cause of llm.cache.misses (missCause).
	firstOnThread bool

	computeMu sync.Mutex
	computed  bool
	// released is set when Compute gave up on the request (a skip, a timeout, no record): the
	// answer is "no figure" and stays so.
	released bool
	result   accountant.Result
	ok       bool
}

// RequestStart implements pipeline.RequestAccountant. It reads pctx.BillingSnapshot() — captured
// by the FIRST Pipeline.Run call on this pctx, before any plugin of either direction can rewrite
// the credential header BillingFrom reads — and stores the billing mode it implies for
// RequestEnd. It does NOT call accountant.BillingFrom itself: this hook runs on the outbound
// Pipeline, which on every listener runs its Run AFTER the inbound chain's own Run on the same
// pctx, so reading the header fresh here would see jwt-validation's placeholder_mode or
// token-exchange's rewrite of Authorization instead of the client's own credential — silently
// misclassifying the billing mode as "unknown" and zeroing every saving's dollar total for the
// request. BillingSnapshot's own doc comment has the detail.
//
// accountant.BillingFrom reads only the credential's type and returns no part of the credential
// itself (accountant R2); only the resulting mode is stored, never the header value.
func (h *Host) RequestStart(pctx *pipeline.Context) {
	if h == nil || pctx == nil {
		return
	}
	billing := pctx.BillingSnapshot()
	pipeline.SetState(pctx, billingKey, &billing)
}

// RequestEnd implements pipeline.RequestAccountant. It takes what the record needs from pctx and
// starts the record on a goroutine of its own: the parse, BeginRequest, each op and EndRequest
// never run on the request goroutine (global constraint).
//
// Traffic that is not an inference request, or that has no session to key a thread on, starts
// nothing and logs nothing (Review Focus 2). The one exception is a request on a path FormatOf does
// not know that an operator still reported ops on: it gets one DEBUG line, because its saving is
// lost.
//
// A request a plugin asked to translate (pctx.TranslateTarget) also starts nothing: the listener
// converts the body to the target protocol in startTranslation, which runs after Run returns, so
// at this point pctx.Body is still the client-protocol body. wire.FormatOf(pctx.Path) and
// facts.Classify both key on the client's own path, so a record built here would classify and
// price a request under the wrong protocol's caching rules — the provider bills the converted
// body under its own. The same TranslateTarget check also covers a translation the listener
// later refuses: RequestEnd has no later chance to undo the record it would otherwise start, and
// a refused request must start no accounting work (pipeline/accounting.go).
func (h *Host) RequestEnd(pctx *pipeline.Context) {
	if h == nil || pctx == nil || pctx.SessionID == "" {
		return
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		return
	}
	format, ok := wire.FormatOf(pctx.Path)
	if !ok {
		// An operator acted on a path the accountant cannot read: its saving goes unrecorded.
		// Said once per request at DEBUG, so a paths: config wider than FormatOf is visible.
		if n := len(pctx.ReportedOps()); n > 0 {
			slog.Debug("accounting: unknown wire format; saving not recorded", "path", pctx.Path, "ops", n)
		}
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
		// R55: the first request after a client compaction. The library records it in
		// Result.Compaction and prices nothing from it.
		Compaction: pctx.ClientCompaction(),
	}
	thread := ThreadOf(pctx)
	seen := h.setThreadBilling(pctx.SessionID, thread.ID, billing.Mode)
	if h.metaHook != nil {
		h.metaHook(meta)
	}
	p := &pending{done: make(chan struct{}), meta: meta, firstOnThread: !seen}
	pipeline.SetState(pctx, pendingKey, p)
	go h.end(p, format, meta, client, forwarded, ops, thread, facts.Classify(pctx.Path))
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
// pushed it over threadBillingCap. Runs on the request goroutine, inside RequestEnd. seen says
// whether the thread already had an entry: the check and the write are under one lock, so two
// concurrent requests cannot both be the thread's first.
func (h *Host) setThreadBilling(session, thread string, mode accountant.BillingMode) (seen bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.threadBilling == nil {
		h.threadBilling = map[threadBillingKey]threadBillingEntry{}
	}
	key := threadBillingKey{session: session, thread: thread}
	_, seen = h.threadBilling[key]
	h.threadBilling[key] = threadBillingEntry{
		mode:     mode,
		lastSeen: h.now(),
	}
	h.trimThreadBillingLocked()
	return seen
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
	// The prefix another thread of the session kept warm: the ping did not keep it alive, so
	// the rescue leaves it out (accountant R54, #2138). Every request is recorded; the bodies
	// are compared only when this one carries ping facts, the only time the library reads it.
	f.SharedWarm = h.siblings.sharedWarm(meta, thread.ID, f.TTL, fwdBody, fwdBPs, p.pinged)
	if h.factsHook != nil {
		h.factsHook(f)
	}
	req.EndRequest(thread, fwdBody, fwdBPs, f)
	h.dirty.Store(true)
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
	// Held for the whole call, not just the check-and-set: a concurrent second caller blocks
	// here until the first finishes (up to computeWait) and then reads its cached answer,
	// rather than both passing the check and racing into computeSafe (see pending's doc comment).
	p.computeMu.Lock()
	defer p.computeMu.Unlock()
	if p.computed {
		return p.result, p.ok
	}
	p.computed = true

	in := SkipInput{Status: pctx.StatusCode, Rounds: 1 + len(pctx.ResendReasons()), SessionID: pctx.SessionID}
	if why := SkipReason(in); why != "" {
		h.release(p, why)
		return accountant.Result{}, false
	}
	wait := h.computeWait
	if wait <= 0 {
		wait = computeWait
	}
	timer := time.NewTimer(wait)
	select {
	case <-p.done:
		timer.Stop()
	case <-timer.C:
		slog.Warn("accounting: the record was not ended in time; the request is recorded with no saving",
			"wait", wait)
		h.release(p, "timeout")
		return accountant.Result{}, false
	}
	in.ServerHeldHistory = p.serverHeld
	if why := SkipReason(in); why != "" {
		h.release(p, why)
		return accountant.Result{}, false
	}
	inf := pctx.Extensions.Inference
	if p.req == nil || inf == nil {
		h.release(p, "no_record")
		return accountant.Result{}, false
	}
	// COUNTS NO REQUEST COULD HAVE REPORTED are refused, as settle refuses them: a negative or
	// past-the-ceiling counter (pricing.PlausibleUsage) would be priced into an impossible saving.
	if !pricing.PlausibleUsage(pricing.UsageFromInference(inf)) {
		h.release(p, "usage_implausible")
		return accountant.Result{}, false
	}
	// CacheWrite1h is the part of CacheWrite billed at the 1-hour rate, never an
	// addition to it (accountant.Usage's own doc).
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
	h.dirty.Store(true)
	p.req = nil
	if p.ok && p.pinged {
		h.dropPing(p.ping.session, p.ping.thread, p.pingWindow)
	}
	if p.ok {
		// Once per request: Compute is latched above. Priced at the endpoint and prompt total
		// Host.Savings prices the ledger's figure at, so the two agree.
		endpoint := pctx.Host
		if _, redirected, ok := pctx.RedirectTarget(); ok {
			endpoint = redirected
		}
		h.observe(p.meta, usage, inf.OutputTokens, p.result, endpoint, pricing.UsageFromInference(inf).PromptTotal(), p.firstOnThread)
	}
	return p.result, p.ok
}

// release marks a request's handle released: Compute will not run for it. Callers hold
// p.computeMu.
func (h *Host) release(p *pending, why string) {
	// Only the handle goes: the record's goroutine may still be running, and it ends on its own.
	// Once it has, nothing references the library record.
	//
	// The pctx map is NOT written here. Compute reads pendingKey before it takes computeMu, so a
	// delete from a concurrent caller's skip path would be a map read racing a map write — a fatal
	// error in Go, not just a -race report. The handle stays in the map; the
	// computed latch (set before any release) answers every later call with "no figure".
	p.released = true
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

// modelChangePrefix starts the ChangeID of every modelChangeOp, so missCause can tell a break a
// plugin's model change caused from any other.
const modelChangePrefix = "model:"

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
		ChangeID: modelChangePrefix + from + "->" + to,
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
