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
	Upstream *http.Client
	CAPEM    []byte
}
