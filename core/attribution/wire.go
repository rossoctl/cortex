package attribution

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The headers an Attribution will travel in: what a hop that resolved a request — a local relay
// that ran Resolve at accept time — stamps on it for the recorder behind it, which then trusts
// the stamp instead of resolving again. That is how attribution can run outside the proxy that
// records: the relay owns the process lookup, the recorder owns the events.
//
// STRIP, THEN INJECT. A client can send these headers too, and one that did would choose its
// own session and process. So the forward proxy removes them from what the client sent (Strip),
// and Encode strips before it sets. Only a hop the deployment trusts may set them, and Decode
// is for that hop's requests alone. No listener calls Encode or Decode yet.
const (
	HeaderAgent = "X-Cortex-Agent"
	HeaderProc  = "X-Cortex-Proc"
	// HeaderClientProc is ClientProc.
	HeaderClientProc = "X-Cortex-Client-Proc"
	HeaderSession    = "X-Cortex-Session"
	HeaderSource     = "X-Cortex-Source"
	// HeaderNamedBy is the session-id header that named HeaderSession, when one did.
	HeaderNamedBy = "X-Cortex-Session-Header"
	// HeaderChain is ChainSession.
	HeaderChain = "X-Cortex-Chain-Session"
)

var wireHeaders = []string{HeaderAgent, HeaderProc, HeaderClientProc, HeaderSession, HeaderSource, HeaderNamedBy, HeaderChain}

// Strip removes every attribution header from h, so a client cannot assert one.
func Strip(h http.Header) {
	for _, k := range wireHeaders {
		h.Del(k)
	}
}

// Encode sets a's headers on h. It first removes every attribution header (Strip), then
// sets one header for each non-empty field. So a value the client sent never survives.
func Encode(h http.Header, a Attribution) {
	Strip(h)
	for k, v := range map[string]string{
		HeaderAgent: a.Agent, HeaderProc: a.Proc, HeaderClientProc: a.ClientProc, HeaderSession: a.Session,
		HeaderSource: string(a.Source), HeaderNamedBy: a.Header, HeaderChain: a.ChainSession,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
}

// Decode reads an Attribution that a trusted hop set on h. ok is false when h carries none,
// or carries one that is malformed — a source Resolve does not return, a session id the store
// would refuse, a process that is not "<pid>.<start>", a control rune — and then the caller
// resolves the request itself. Call it only for a hop the deployment trusts.
func Decode(h http.Header) (Attribution, bool) {
	if h.Get(HeaderSource) == "" && h.Get(HeaderSession) == "" && h.Get(HeaderProc) == "" {
		return Attribution{}, false
	}
	a := Attribution{
		Agent:        h.Get(HeaderAgent),
		Proc:         h.Get(HeaderProc),
		ClientProc:   h.Get(HeaderClientProc),
		Session:      h.Get(HeaderSession),
		Source:       pipeline.SessionSource(h.Get(HeaderSource)),
		Header:       h.Get(HeaderNamedBy),
		ChainSession: h.Get(HeaderChain),
	}
	switch a.Source {
	case "", pipeline.SessionSourceHeader, pipeline.SessionSourceProcess, pipeline.SessionSourcePending,
		pipeline.SessionSourceAffinity:
	default:
		return Attribution{}, false
	}
	for _, id := range []string{a.Session, a.ChainSession} {
		if len(id) > session.MaxSessionIDLen {
			return Attribution{}, false
		}
	}
	for _, p := range []string{a.Proc, a.ClientProc} {
		if p != "" && !validProc(p) {
			return Attribution{}, false
		}
	}
	for _, s := range []string{a.Agent, a.Header, a.Session, a.ChainSession} {
		if strings.ContainsFunc(s, isControl) {
			return Attribution{}, false
		}
	}
	return a, true
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// validProc reports whether p is Proc.Key's form: a pid, a dot, a start time.
func validProc(p string) bool {
	pid, start, ok := strings.Cut(p, ".")
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(pid, 10, 32); err != nil {
		return false
	}
	_, err := strconv.ParseInt(start, 10, 64)
	return err == nil
}
