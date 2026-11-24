package contextguru

import (
	"net/http"
	"testing"
	"time"
)

// On a local install the plugin's model calls never route through HTTPS_PROXY: a call sent back
// into Cortex's own forward proxy would be priced as agent traffic (costMicros) and also recorded
// as the plugin's spend (pluginSpendMicros). DefaultTransport's Proxy is ProxyFromEnvironment,
// which is what this pins off.
func TestModelHTTPClient_LocalInstallNeverUsesTheEnvironmentProxy(t *testing.T) {
	c := modelHTTPClient(3*time.Second, true)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the local model client's transport has a Proxy func; HTTPS_PROXY would route the call back into Cortex")
	}
	if c.Timeout != 3*time.Second || c.CheckRedirect == nil {
		t.Errorf("client = timeout %v, redirect policy set %v; want 3s and the sentinel redirect policy",
			c.Timeout, c.CheckRedirect != nil)
	}
	if modelHTTPClient(0, true).Transport != c.Transport {
		t.Error("each client got its own transport; the plugin's calls should share one pool")
	}
	// The process default keeps its environment proxy: only this plugin's clients are pinned.
	if d, ok := http.DefaultTransport.(*http.Transport); ok && d.Proxy == nil {
		t.Error("http.DefaultTransport lost its Proxy func: the pin leaked into the process default")
	}
}

// Outside a local install the environment proxy is kept: in a cluster a sidecar's only route to an
// external model may be an HTTPS_PROXY egress proxy.
func TestModelHTTPClient_SharedDeploymentKeepsTheEnvironmentProxy(t *testing.T) {
	c := modelHTTPClient(3*time.Second, false)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy == nil {
		t.Error("the shared model client's transport has no Proxy func; an egress proxy in HTTPS_PROXY would be bypassed")
	}
	if modelHTTPClient(0, false).Transport != c.Transport {
		t.Error("each client got its own transport; the plugin's calls should share one pool")
	}
	if modelHTTPClient(0, true).Transport == c.Transport {
		t.Error("the local and shared clients share a transport; one of them has the wrong Proxy")
	}
}
