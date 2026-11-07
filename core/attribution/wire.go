package attribution

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
// resolves the request itself. Call it only for a hop the deployment trusts. An empty
// Attribution{} cannot travel: Encode sets no header for it, and Decode reads that as none.
func Decode(h http.Header) (Attribution, bool) {
	present := false
	for _, k := range wireHeaders {
		present = present || h.Get(k) != ""
	}
	if !present {
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
		// The rule session.validHeaderSessionID applies: valid UTF-8, no C0, DEL or C1 rune.
		if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
			return Attribution{}, false
		}
	}
	return a, true
}

// validProc reports whether p is exactly what Proc.Key writes: a pid, a dot, a start time,
// neither negative. A decoded Proc is a bucket key, so "01.2" must not pass as "1.2".
func validProc(p string) bool {
	pidS, startS, ok := strings.Cut(p, ".")
	if !ok {
		return false
	}
	pid, err := strconv.ParseInt(pidS, 10, 32)
	if err != nil || pid < 0 {
		return false
	}
	start, err := strconv.ParseInt(startS, 10, 64)
	if err != nil || start < 0 {
		return false
	}
	return Proc{PID: int32(pid), Start: start}.Key() == p
}
