package attribution

import "net/http"

// The headers an Attribution will travel in: what a hop that resolved a request — a local relay
// that ran Resolve at accept time — stamps on it for the recorder behind it, which then trusts
// the stamp instead of resolving again. That is how attribution can run outside the proxy that
// records: the relay owns the process lookup, the recorder owns the events.
//
// STRIP, THEN INJECT. A client can send these headers too, and one that did would choose its
// own session and process. So every listener removes them from what the client sent (Strip)
// today, before any hop is trusted to set them. The encoder and decoder land with that hop.
const (
	HeaderAgent   = "X-Cortex-Agent"
	HeaderProc    = "X-Cortex-Proc"
	HeaderSession = "X-Cortex-Session"
	HeaderSource  = "X-Cortex-Source"
	// HeaderNamedBy is the session-id header that named HeaderSession, when one did.
	HeaderNamedBy = "X-Cortex-Session-Header"
	// HeaderChain is ChainSession.
	HeaderChain = "X-Cortex-Chain-Session"
)

var wireHeaders = []string{HeaderAgent, HeaderProc, HeaderSession, HeaderSource, HeaderNamedBy, HeaderChain}

// Strip removes every attribution header from h, so a client cannot assert one.
func Strip(h http.Header) {
	for _, k := range wireHeaders {
		h.Del(k)
	}
}
