package contextguru

import (
	"net/http"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/llmclient"
)

// newModelTransport is http.DefaultTransport cloned with llmclient's 100-connection idle pool per
// host. If noProxy, the clone's Proxy is pinned off; otherwise it keeps DefaultTransport's
// ProxyFromEnvironment.
func newModelTransport(noProxy bool) *http.Transport {
	var tr *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	} else {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 100}
	}
	if noProxy {
		tr.Proxy = nil
	}
	tr.MaxIdleConnsPerHost = 100
	return tr
}

// The two transports the plugin's model calls use, one per kind of deployment, each built once so
// all of the plugin's clients share its pool.
//
// PROXY PINNED OFF ON A LOCAL INSTALL. A laptop shell that routes agents through Cortex sets
// HTTPS_PROXY to Cortex's own listener, and a Cortex process started from it inherits that. With
// ProxyFromEnvironment, the plugin's summary and extract calls would go back into Cortex's own
// forward proxy. There inference-parser prices the call as agent traffic (CostMicros), while the
// accountant also records it as the plugin's spend (Event.Spends, PluginSpendMicros): one call in
// two totals. The sentinel header stops the plugin acting on its own call; it does not stop the
// recording.
//
// PROXY KEPT IN A CLUSTER. There a sidecar's only route to an external model may be an egress
// proxy named by HTTPS_PROXY, so pinning it off would make every model call fail and degrade to
// deterministic.
var (
	localModelTransport  = sync.OnceValue(func() *http.Transport { return newModelTransport(true) })
	sharedModelTransport = sync.OnceValue(func() *http.Transport { return newModelTransport(false) })
)

// modelTransport is the shared transport for this kind of deployment: see localModelTransport.
func modelTransport(localInstall bool) *http.Transport {
	if localInstall {
		return localModelTransport()
	}
	return sharedModelTransport()
}

// modelHTTPClient is the http.Client for one llmclient.Client the plugin builds: the transport for
// this kind of deployment (see localModelTransport), the per-call timeout, and llmclient's sentinel
// redirect policy, which llmclient installs only on the client it builds itself.
func modelHTTPClient(timeout time.Duration, localInstall bool) *http.Client {
	if timeout <= 0 {
		timeout = llmclient.DefaultTimeout
	}
	tr := sharedModelTransport()
	if localInstall {
		tr = localModelTransport()
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: llmclient.SentinelRedirectPolicy(sentinelHeader),
		Transport:     modelTransport(localInstall),
	}
}
