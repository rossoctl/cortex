package accounting

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// PingPurpose is the cost record's Purpose for a keep-alive ping (event.PurposeKeepAlive), so a
// later consumer can show ping spend apart from agent spend (design §5.8). The record's Source
// stays what settle set: how the cost was arrived at.
const PingPurpose = event.PurposeKeepAlive

// pingFactsCap and pingFactsTrimTo bound Host.pingFacts the way threadBillingCap bounds
// threadBilling: one entry per (session, thread) that was pinged and has not yet made its next
// priced request.
const (
	pingFactsCap    = threadBillingCap
	pingFactsTrimTo = threadBillingTrimTo
)

// PingInfo is what the host knows about a ping beyond accountant.Ping: the library type carries
// neither the response status nor whether the user asked for the ping.
type PingInfo struct {
	// Status is the ping's HTTP status. A ping outside 200-299 is refused.
	Status int
	// Manual marks a ping the user asked for. It is priced, but its facts are not kept (see Ping).
	Manual bool
	// Duration is how long the ping took, for the cost event.
	Duration time.Duration
}

// pingEntry is one Host.pingFacts value: the facts of the thread's latest ping, and when they
// were stored, for the trim.
type pingEntry struct {
	facts accountant.PingFacts
	at    time.Time
}

// pingOps and computeStandalone are the two library calls Ping makes. Tests replace pingOps to
// see the billing it is given, or to make it panic.
var (
	pingOps           = accountant.PingOps
	computeStandalone = accountant.ComputeStandalone
)

// Ping accounts for one keep-alive ping: a spend on the thread it kept warm, and the facts the
// thread's next request needs to credit the rescue under the same ChangeID (design §5.8, wiring
// W4.6). Ported from the context-guru proxy's PingSavings, minus the dashboard.
//
// It refuses (ok false, nothing recorded, nothing kept) a ping with no Window — its ChangeID
// would be shared by every such ping —, a status outside 200-299, or no usage: it billed
// nothing. The billing mode is that of the thread's last request (design §5.7), because a ping
// uses the same credential.
//
// A manual ping is priced and recorded, but its facts are not kept: the write it avoided is
// already booked on its own row from the user's estimate, and a library rescue on the next
// request would credit the same write again (context-guru#768 B3).
//
// The ping's spend is written to the cost recorders (the ledger writer and the usage
// aggregator) as a response event whose cost record has Purpose PingPurpose. It never goes through the session
// store: a ping is not agent activity, and counting it as activity would distort the idle
// tracking keep-alive depends on.
//
// host is the upstream the ping was sent to; it prices the ping and labels its event. A panic in
// the library is recovered and reported as not ok: the keeper calls this on its own goroutine.
func (h *Host) Ping(sessionID string, client *model.EventClient, host string, p accountant.Ping, info PingInfo) (res accountant.Result, ok bool) {
	if h == nil {
		return accountant.Result{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accounting: ping pricing panicked; the ping is recorded with no saving", "panic", r)
			res, ok = accountant.Result{}, false
		}
	}()
	u := p.Usage
	if p.Window.IsZero() || info.Status < 200 || info.Status > 299 ||
		u.Input+u.CacheRead+u.CacheWrite+u.Output == 0 {
		return accountant.Result{}, false
	}
	// The zero check above lets mixed-sign counters that sum to zero (or an outright negative
	// count) through; Compute refuses those via pricing.PlausibleUsage, and a ping must too
	// — otherwise computeStandalone/Units/observe below can book negative spend
	// or savings from a count no provider could actually have sent.
	if !pricing.PlausibleUsage(pricing.Usage{
		Input: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output,
		CacheWrite1h: u.CacheWrite1h,
	}) {
		return accountant.Result{}, false
	}
	if sessionID == "" {
		sessionID = p.Session
	}
	p.Session = sessionID
	if p.Host == "" {
		p.Host = host
	}
	thread := pingThread(p.ThreadID)
	// One read, so the library, the cost record and the sink all see the same mode.
	billing := h.ThreadBilling(sessionID, thread)
	ops, facts := pingOps(p, billing)
	res = computeStandalone(ops)
	if !info.Manual {
		h.putPing(sessionID, thread, facts)
	}
	figs := h.recordPing(sessionID, client, host, p, info, billing.Mode)
	h.notifyPing(PingSpend{Session: sessionID, Thread: thread, Model: p.Model, Host: host, At: p.At,
		Usage: p.Usage, Figures: figs})
	return res, true
}

// PingSpend is one accounted keep-alive ping, as a ping listener receives it: the tokens it
// billed and its spend split by basis (only the KeepAlive* fields of Figures are set; all zero
// when the ping was not priced).
type PingSpend struct {
	Session, Thread, Model, Host string
	At                           time.Time
	Usage                        accountant.CallUsage
	Figures                      event.Figures
}

// PingListener receives every ping Host.Ping accounts. It is called on the keeper's goroutine,
// after the ping is recorded, with no Host lock held; it must not block. A panic in it is
// recovered and logged, and the other listeners still run.
type PingListener func(PingSpend)

// pingListenerEntry is one registered ping listener. key is "" for AddPingListener's.
type pingListenerEntry struct {
	key string
	fn  PingListener
}

// AddPingListener registers fn for every ping Host.Ping accounts from now on, and returns the
// function that removes it. A ping is not agent activity, so it never passes through the session
// store or the pipeline; this is how the consumers that must still count its cost see it. For a
// process-wide consumer (cmd/cortex's session store). A plugin uses AddKeyedPingListener.
func (h *Host) AddPingListener(fn PingListener) (remove func()) { return h.addPingListener("", fn) }

// AddKeyedPingListener is AddPingListener for a consumer that a config reload builds again:
// of the live registrations under one key, ONLY THE NEWEST IS CALLED. The reloader starts the new
// pipeline (its plugins' Init) before it stops the old one, and the old one can drain for minutes;
// with two plain listeners, a consumer that writes shared state (session-budget's Redis counters)
// would count every ping twice for that time. Removing the newest registration gives the turn back
// to the one before it, so a reload that fails after the new instance's Init leaves the old
// instance counting. Removing an older one only drops it. key names what the consumer counts
// into, so two consumers that share it are one counter; an empty key is a plain listener.
func (h *Host) AddKeyedPingListener(key string, fn PingListener) (remove func()) {
	return h.addPingListener(key, fn)
}

func (h *Host) addPingListener(key string, fn PingListener) (remove func()) {
	if h == nil || fn == nil {
		return func() {}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pingListeners == nil {
		h.pingListeners = map[uint64]pingListenerEntry{}
	}
	h.pingListenerSeq++
	id := h.pingListenerSeq
	h.pingListeners[id] = pingListenerEntry{key: key, fn: fn}
	return func() {
		h.mu.Lock()
		delete(h.pingListeners, id)
		h.mu.Unlock()
	}
}

// notifyPing calls every plain ping listener and the newest listener of each key with s, in
// registration order.
func (h *Host) notifyPing(s PingSpend) {
	h.mu.Lock()
	newest := map[string]uint64{}
	ids := make([]uint64, 0, len(h.pingListeners))
	for id, e := range h.pingListeners {
		if e.key == "" {
			ids = append(ids, id)
		} else if id > newest[e.key] {
			newest[e.key] = id
		}
	}
	for _, id := range newest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	fns := make([]PingListener, 0, len(ids))
	for _, id := range ids {
		fns = append(fns, h.pingListeners[id].fn)
	}
	h.mu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("accounting: ping listener panicked; the other listeners still run", "panic", r)
				}
			}()
			fn(s)
		}()
	}
}

// pingThread is the accountant's id for a ping's thread: "" is the primary thread, "main", as
// accountant.PingOps and core/accounting.ThreadOf both name it.
func pingThread(id string) string {
	if id == "" {
		return "main"
	}
	return id
}

// recordPing writes the ping's cost to every cost recorder, priced with the process rate table
// through settle.SettleInput, the same rule a proxied response is priced by.
//
// The record carries the basis of mode, the thread's billing mode (design §5.7a), so the ledger
// and /v1/usage book its dollars as money only on an API key — where the metrics sink moves its
// USD counters too. A subscription ping also carries its tokens in usage-limit units (the
// configured weights, output included); every non-usd ping is Estimated.
//
// It returns the record's Figures (its keep-alive spend by basis) for the ping listeners, whether
// or not there is a recorder to write to.
func (h *Host) recordPing(sessionID string, client *model.EventClient, host string, p accountant.Ping, info PingInfo,
	mode accountant.BillingMode) event.Figures {
	h.mu.Lock()
	recs := append([]session.Recorder(nil), h.recorders...)
	h.mu.Unlock()
	inf := &model.InferenceExtension{
		Model:            p.Model,
		InputTokens:      p.Usage.Input,
		CacheReadTokens:  p.Usage.CacheRead,
		CacheWriteTokens: p.Usage.CacheWrite,
		// CacheWrite1h is the part of CacheWrite billed at the 1-hour rate, never an
		// addition to it (p.Usage embeds accountant.Usage, whose own doc says the
		// same). settle.SettleInput below prices this subset at the 1h rate.
		CacheWrite1hTokens: p.Usage.CacheWrite1h,
		OutputTokens:       p.Usage.Output,
	}
	settled := settle.SettleInput(settle.Input{Host: host, Inference: inf}, h.resolver)
	rec := settle.NewRecord(settled, nil)
	rec.Purpose = PingPurpose
	rec.Basis = BasisOf(mode)
	rec.Estimated = rec.Basis != BasisUSD
	if rec.Basis == BasisSubscriptionEquivalent {
		u := p.Usage.Usage
		tt := accountant.TierTokens{Input: u.Input, CacheWrite5m: u.CacheWrite - u.CacheWrite1h,
			CacheWrite1h: u.CacheWrite1h, CacheRead: u.CacheRead}
		rec.Units = Units(h.weights, tt, p.Usage.Output)
	}
	figs := rec.Figures()
	if len(recs) == 0 {
		return figs
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		slog.Debug("accounting: ping cost record not encoded", "error", err)
		return figs
	}
	at := p.At
	if at.IsZero() {
		at = h.now()
	}
	// Each recorder gets its own event: a Recorder may keep the pointer.
	for _, r := range recs {
		ev := &model.SessionEvent{
			SessionID:  sessionID,
			At:         at,
			Direction:  model.Outbound,
			Phase:      model.SessionResponse,
			RequestID:  "keep-alive:" + sessionID + ":" + pingThread(p.ThreadID) + ":" + at.UTC().Format(time.RFC3339Nano),
			Inference:  inf,
			Plugins:    map[string]json.RawMessage{event.Key: raw, event.PluginName: raw},
			StatusCode: info.Status,
			Host:       host,
			Duration:   info.Duration,
			Client:     client,
		}
		r.Record(sessionID, ev)
	}
	return figs
}

// putPing keeps f as the latest ping facts of (session, thread); a second ping overwrites the
// first. Trims the oldest entries once the map passes pingFactsCap.
func (h *Host) putPing(sessionID, thread string, f accountant.PingFacts) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pingFacts == nil {
		h.pingFacts = map[threadBillingKey]pingEntry{}
	}
	h.pingFacts[threadBillingKey{session: sessionID, thread: thread}] = pingEntry{facts: f, at: h.now()}
	if len(h.pingFacts) <= pingFactsCap {
		return
	}
	type aged struct {
		key threadBillingKey
		at  time.Time
	}
	all := make([]aged, 0, len(h.pingFacts))
	for k, e := range h.pingFacts {
		all = append(all, aged{k, e.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(h.pingFacts)-pingFactsTrimTo] {
		delete(h.pingFacts, a.key)
	}
}

// peekPing returns the ping facts of (session, thread) and keeps them. A request reads them in
// its record's goroutine; it drops them (dropPing) only once it is priced, so a request that
// books nothing leaves them for the next one — the proxy's PingStore rule.
func (h *Host) peekPing(sessionID, thread string) (accountant.PingFacts, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.pingFacts[threadBillingKey{session: sessionID, thread: thread}]
	return e.facts, ok
}

// dropPing deletes the facts of (session, thread) if they are still the ones with window: facts
// with another window are a newer ping, stored after the request read its own, and they stay for
// the next request.
func (h *Host) dropPing(sessionID, thread, window string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := threadBillingKey{session: sessionID, thread: thread}
	if e, ok := h.pingFacts[k]; ok && e.facts.PingWindow == window {
		delete(h.pingFacts, k)
	}
}
