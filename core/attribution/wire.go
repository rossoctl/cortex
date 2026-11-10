package attribution

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

// The headers an Attribution will travel in: what a hop that resolved a request — a local relay
// that ran Resolve at accept time — stamps on it for the recorder behind it, which then trusts
// the stamp instead of resolving again. That is how attribution can run outside the proxy that
// records: the relay owns the process lookup, the recorder owns the events.
//
// STRIP, THEN INJECT. A client can send these headers too, and one that did would choose its
// own session and process. So the forward proxy removes them from what the client sent (Strip)
// today, before any hop is trusted to set them. The praxis hop sends one HeaderAttribution
// instead: Wire.Encode is its encoder, and the gateway (praxis/) has the decoder.
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

// HeaderAttribution carries a Wire from the praxis hop to cortex-gateway.
const HeaderAttribution = "X-Cortex-Attribution"

// Wire is the attribution result that the hop sends in X-Cortex-Attribution.
// The JSON keys must stay equal to the Rust Attribution struct in praxis/filters.
type Wire struct {
	Agent         string `json:"agent,omitempty"`
	Proc          string `json:"proc,omitempty"`
	ClientProc    string `json:"clientProc,omitempty"`
	Session       string `json:"session,omitempty"`
	Source        string `json:"source,omitempty"`
	SessionHeader string `json:"sessionHeader,omitempty"`
	ChainSession  string `json:"chainSession,omitempty"`
	RecordSession string `json:"recordSession,omitempty"`
}

// Encode gives the header value: the JSON of w in base64url with no padding.
func (w Wire) Encode() string {
	b, _ := json.Marshal(w) // a struct of strings always marshals
	return base64.RawURLEncoding.EncodeToString(b)
}
