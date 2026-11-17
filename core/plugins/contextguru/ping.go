package contextguru

import (
	"time"

	"github.com/rossoctl/accountant"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// pingEmitter is the keep-alive emitter for the entry a finished request establishes. Each keeper
// Record carries its own emitter, so a ping knows the session, the agent and the upstream it is
// accounted to. It logs each ping, as logEmitter does, and accounts for it through the accountant
// host (design §5.8).
type pingEmitter struct {
	logEmitter
	acct    *accounting.Host
	session string
	client  *pipeline.EventClient
	host    string
}

// pingEmitter is the emitter for the keep-alive entry pctx establishes.
func (p *ContextGuru) pingEmitter(pctx *pipeline.Context, sid string) cgcomponents.Emitter {
	return pingEmitter{acct: p.acct, session: sid, client: pctx.ClientInfo(), host: pctx.Host}
}

// KeepAlivePing logs the ping and hands it to the accountant host, which refuses it, prices it,
// records its spend and keeps its facts for the thread's next request. KeepAliveReport is a
// context-guru type, so the conversion happens here: core/accounting sees only accountant types.
func (e pingEmitter) KeepAlivePing(r cgcomponents.KeepAliveReport) {
	e.logEmitter.KeepAlivePing(r)
	if e.acct == nil {
		return
	}
	sid := e.session
	if sid == "" {
		sid = r.Session
	}
	e.acct.Ping(sid, e.client, e.host, pingFromReport(r, e.host), accounting.PingInfo{
		Status:   r.Status,
		Manual:   r.Manual,
		Duration: time.Duration(r.DurationMs * float64(time.Millisecond)),
	})
}

// pingFromReport maps a keeper report to the library's Ping (wiring W4.6), as the context-guru
// proxy's pingFromReport does. host is the upstream the ping went to: in Cortex the report's Route
// is the request PATH (keepalive.go's recordFor sets it so), so the upstream comes from the
// request instead. r.CostUSD is deliberately not carried: the host prices the ping with its own
// rates. CacheWrite1h is the part of CacheWrite billed at the one-hour rate, in both types.
func pingFromReport(r cgcomponents.KeepAliveReport, host string) accountant.Ping {
	return accountant.Ping{
		Session: r.Session, ThreadID: r.Thread, Model: r.Model, Host: host,
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead),
			CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)}, Output: int(r.Output)},
		At: pingStart(r), Window: r.Window,
	}
}

// pingStart is when the ping began. The report's TS is when it was recorded, after the response,
// and the library measures the entry gap from the START of a request or ping, as the provider
// measures the TTL (PingFacts.LastPing is "when the ping started"). A ping that streams for a
// while would otherwise shorten the gap. A report with no TS falls back to now.
func pingStart(r cgcomponents.KeepAliveReport) time.Time {
	at := time.Now()
	if r.TS > 0 {
		at = time.UnixMilli(r.TS)
	}
	return at.Add(-time.Duration(r.DurationMs * float64(time.Millisecond)))
}
