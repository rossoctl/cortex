package contextguru

import (
	"net/http"
	"testing"
	"time"
)

// The plugin's model calls never route through HTTPS_PROXY: a call sent back into Cortex's own
// forward proxy would be priced as agent traffic (costMicros) and also recorded as the plugin's
// spend (pluginSpendMicros). DefaultTransport's Proxy is ProxyFromEnvironment, which is what this
// pins off.
func TestModelHTTPClient_NeverUsesTheEnvironmentProxy(t *testing.T) {
	c := modelHTTPClient(3 * time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the model client's transport has a Proxy func; HTTPS_PROXY would route the call back into Cortex")
	}
	if c.Timeout != 3*time.Second || c.CheckRedirect == nil {
		t.Errorf("client = timeout %v, redirect policy set %v; want 3s and the sentinel redirect policy",
			c.Timeout, c.CheckRedirect != nil)
	}
	if modelHTTPClient(0).Transport != c.Transport {
		t.Error("each client got its own transport; the plugin's calls should share one pool")
	}
	// The process default keeps its environment proxy: only this plugin's clients are pinned.
	if d, ok := http.DefaultTransport.(*http.Transport); ok && d.Proxy == nil {
		t.Error("http.DefaultTransport lost its Proxy func: the pin leaked into the process default")
	}
}
