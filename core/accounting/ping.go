package accounting

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// PingSource is the cost record's Source for a keep-alive ping, so /v1/usage and agentop can
// show ping spend apart from agent spend (design §5.8).
const PingSource = "keep-alive"

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
// aggregator) as a response event with Source PingSource. It never goes through the session
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
	if sessionID == "" {
		sessionID = p.Session
	}
	p.Session = sessionID
	if p.Host == "" {
		p.Host = host
	}
	thread := pingThread(p.ThreadID)
	ops, facts := pingOps(p, h.ThreadBilling(sessionID, thread))
	res = computeStandalone(ops)
	if !info.Manual {
		h.putPing(sessionID, thread, facts)
	}
	h.recordPing(sessionID, client, host, p, info)
	return res, true
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
func (h *Host) recordPing(sessionID string, client *model.EventClient, host string, p accountant.Ping, info PingInfo) {
	h.mu.Lock()
	recs := append([]session.Recorder(nil), h.recorders...)
	h.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	inf := &model.InferenceExtension{
		Model:            p.Model,
		InputTokens:      p.Usage.Input,
		CacheReadTokens:  p.Usage.CacheRead,
		CacheWriteTokens: p.Usage.CacheWrite,
		OutputTokens:     p.Usage.Output,
	}
	settled := settle.SettleInput(settle.Input{Host: host, Inference: inf}, h.resolver)
	rec := settle.NewRecord(settled, nil)
	rec.Source = PingSource
	raw, err := json.Marshal(rec)
	if err != nil {
		slog.Debug("accounting: ping cost record not encoded", "error", err)
		return
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
