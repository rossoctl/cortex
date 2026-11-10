package forwardproxy

import (
	"crypto/subtle"
	"net/http"
)

// The hop headers between Cortex, cortex-gateway and the egress listener
// (praxis mode, #1721).
const (
	// headerAttribution has the same value as attribution.HeaderAttribution.
	// Use that constant here when it lands in core/attribution.
	headerAttribution = "X-Cortex-Attribution"
	headerDest        = "X-Cortex-Dest"
	headerToken       = "X-Cortex-Token"
	headerRequestID   = "X-Cortex-Request-Id"
	headerProbe       = "X-Cortex-Probe"
	headerEgressError = "X-Cortex-Egress-Error"
	// probeDest has no "://", so it can never name an origin (Decision 2).
	probeDest = "cortex-probe"
)

// clientHopHeaders are the five hop headers a client must not send in praxis mode.
var clientHopHeaders = []string{headerAttribution, headerDest, headerToken, headerRequestID, headerProbe}

// stripHopHeaders removes clientHopHeaders from h.
func stripHopHeaders(h http.Header) {
	for _, k := range clientHopHeaders {
		h.Del(k)
	}
}

// tokenMatches compares in constant time. An empty token matches nothing.
func tokenMatches(given, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(given), []byte(token)) == 1
}
