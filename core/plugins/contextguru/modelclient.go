package contextguru

import (
	"net/http"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/llmclient"
)

// modelTransport is the one transport the plugin's model calls use: http.DefaultTransport cloned
// with the proxy pinned off and llmclient's 100-connection idle pool per host.
//
// PROXY PINNED OFF. llmclient's default
// transport is a clone of DefaultTransport, whose Proxy is ProxyFromEnvironment, so a Cortex
// process started with HTTPS_PROXY in its environment — a laptop shell that routes agents through
// Cortex — would send the plugin's summary and extract calls back into Cortex's own forward
// proxy. There inference-parser prices the call as agent traffic (CostMicros), while the
// accountant also records it as the plugin's spend (Event.Spends, PluginSpendMicros): one call in
// two totals. The sentinel header stops the plugin acting on its own call; it does not stop the
// recording. Only this plugin's clients are pinned: ibac and sparc may need an egress proxy in a
// cluster, and their calls are not in Spends.
var modelTransport = sync.OnceValue(func() *http.Transport {
	var tr *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	} else {
		tr = &http.Transport{MaxIdleConnsPerHost: 100}
	}
	tr.Proxy = nil
	tr.MaxIdleConnsPerHost = 100
	return tr
})

// modelHTTPClient is the http.Client for one llmclient.Client the plugin builds: the shared
// no-proxy transport, the per-call timeout, and llmclient's sentinel redirect policy, which
// llmclient installs only on the client it builds itself.
func modelHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = llmclient.DefaultTimeout
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: llmclient.SentinelRedirectPolicy(sentinelHeader),
		Transport:     modelTransport(),
	}
}
