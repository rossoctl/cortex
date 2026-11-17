package contextguru

import (
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/llmclient"
)

// modelTransport returns the one transport the plugin's model calls use for local: a clone of
// http.DefaultTransport with llmclient's 100-connection idle pool per host, one per answer of
// local, shared by every client of that kind.
//
// ON A LOCAL INSTALL THE PROXY IS PINNED OFF, as the keep-alive dispatcher pins its own
// (newPingClient). llmclient's default transport is a clone of DefaultTransport, whose Proxy is
// ProxyFromEnvironment, and on a laptop HTTPS_PROXY points at Cortex itself — the shell routes
// agents through it — so the plugin's summary and extract calls would come back into Cortex's own
// forward proxy. There inference-parser prices the call as agent traffic (CostMicros), while the
// accountant also records it as the plugin's spend (Event.Spends, PluginSpendMicros): one call in
// two totals. The sentinel header stops the plugin acting on its own call; it does not stop the
// recording.
//
// OFF A LOCAL INSTALL ProxyFromEnvironment STAYS. In a cluster HTTPS_PROXY is normally the
// cluster's own egress proxy, and where egress is mandatory a call that ignored it would fail.
// The cost of that choice: a pod whose HTTPS_PROXY points at its own Cortex sidecar double
// counts the plugin's model calls, as above (docs/context-guru-plugin.md).
func modelTransport(local bool) *http.Transport {
	if local {
		return localModelTransport()
	}
	return envModelTransport()
}

var (
	localModelTransport = sync.OnceValue(func() *http.Transport { return newModelTransport(nil) })
	envModelTransport   = sync.OnceValue(func() *http.Transport { return newModelTransport(http.ProxyFromEnvironment) })
)

func newModelTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	var tr *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	} else {
		tr = &http.Transport{MaxIdleConnsPerHost: 100}
	}
	tr.Proxy = proxy
	tr.MaxIdleConnsPerHost = 100
	return tr
}

// modelHTTPClient is the http.Client for one llmclient.Client the plugin builds: the shared
// transport for local (modelTransport), the per-call timeout, and llmclient's sentinel redirect
// policy, which llmclient installs only on the client it builds itself.
func modelHTTPClient(timeout time.Duration, local bool) *http.Client {
	if timeout <= 0 {
		timeout = llmclient.DefaultTimeout
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: llmclient.SentinelRedirectPolicy(sentinelHeader),
		Transport:     modelTransport(local),
	}
}
