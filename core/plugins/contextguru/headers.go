package contextguru

import (
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

// ownHeaderPrefix starts every header that context-guru reads from a client and that is not the
// upstream's: x-context-guru-request-kind (agent.HeaderRequestKind), x-context-guru-client
// (agent/pi) and the others. The plugin's own sentinel (sentinelHeader) has the prefix too.
const ownHeaderPrefix = "X-Context-Guru-"

// stripOwnHeaders removes every x-context-guru-* header from the request before it goes upstream.
// context-guru's host contract (agent/agent.go) asks a host to do this, as context-guru-proxy's
// copyHeaders does: the headers talk to context-guru, and a provider must not see them.
//
// It runs on pctx.Headers, the headers the listener forwards: the forward proxy sends them as they
// are, and the ext_proc listener turns a removed header into a header mutation. The keep-alive keeper
// keeps the same header map for its pings, so a ping does not send them either. The plugin's own
// calls (selfCall) keep the sentinel: OnRequest returns before this runs on them.
func stripOwnHeaders(pctx *pipeline.Context) {
	for k := range pctx.Headers {
		if len(k) >= len(ownHeaderPrefix) && strings.EqualFold(k[:len(ownHeaderPrefix)], ownHeaderPrefix) {
			pctx.Headers.Del(k)
		}
	}
}
