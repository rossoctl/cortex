package tlsbridge

import (
	"net/http"
)

// Engine bundles everything the forward proxy needs to bridge TLS.
// A nil *Engine means the bridge is disabled.
type Engine struct {
	Decision *Decision
	Term     *Terminator
	Skip     *SkipSet
	// Programs is the skip set keyed by client program (Program.Key), which the forward
	// proxy records against instead of Skip whenever it can name the client's program —
	// or by the client process (Program.ProcessKey), when that process started before
	// the CA. Nil leaves every connection to Skip, as before programs could be named.
	Programs *SkipSet
	Upstream *http.Client
	CAPEM    []byte

	// CAFile is the on-disk trust anchor clients must load. Diagnostics only:
	// the bridge itself works from CAPEM. It exists so a listener that notices
	// nothing is being decrypted can name the exact file to trust, which is
	// the single most common cause of that state.
	CAFile string
}
