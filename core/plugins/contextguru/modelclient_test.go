package contextguru

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// The plugin's model calls never route through HTTPS_PROXY: a call sent back into Cortex's own
// forward proxy would be priced as agent traffic (costMicros) and also recorded as the plugin's
// spend (pluginSpendMicros). DefaultTransport's Proxy is ProxyFromEnvironment, which is what this
// pins off.
func TestModelHTTPClient_NeverUsesTheEnvironmentProxyOnALocalInstall(t *testing.T) {
	c := modelHTTPClient(3*time.Second, true)
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
	if modelHTTPClient(0, true).Transport != c.Transport {
		t.Error("each client got its own transport; the plugin's calls should share one pool")
	}
	// The process default keeps its environment proxy: only this plugin's clients are pinned.
	if d, ok := http.DefaultTransport.(*http.Transport); ok && d.Proxy == nil {
		t.Error("http.DefaultTransport lost its Proxy func: the pin leaked into the process default")
	}
}

// Off a local install, HTTPS_PROXY is the cluster's own egress proxy, not Cortex, and a call that
// ignored it would fail where egress is mandatory. So the model client keeps
// ProxyFromEnvironment there; only a local install, where HTTPS_PROXY points at Cortex itself,
// pins it off.
func TestModelHTTPClient_KeepsTheEnvironmentProxyOffALocalInstall(t *testing.T) {
	c := modelHTTPClient(3*time.Second, false)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if !isEnvProxy(tr.Proxy) {
		t.Error("the model client off a local install does not use ProxyFromEnvironment; a mandatory egress proxy is bypassed")
	}
	if modelHTTPClient(0, false).Transport != c.Transport {
		t.Error("each client got its own transport; the plugin's calls should share one pool")
	}
	if modelHTTPClient(0, true).Transport == c.Transport {
		t.Error("the local and cluster clients share one transport; one of them has the wrong proxy")
	}
}

// The plugin passes what BuildWithDeps told it (plugins.LocalInstallConsumer) to every client it
// builds: the keep-alive ping client here, the model clients through the same flag.
func TestPlugin_ProxyFollowsLocalInstall(t *testing.T) {
	for _, local := range []bool{true, false} {
		p := New()
		p.SetLocalInstall(local)
		if err := p.Configure([]byte(`{"context_window":{"model_info":"off"},"keep_alive":{"enabled":true}}`)); err != nil {
			t.Fatal(err)
		}
		tr := p.disp.client.Transport.(*http.Transport)
		if got := tr.Proxy == nil; got != local {
			t.Errorf("local=%v: ping client Proxy nil = %v, want %v", local, got, local)
		}
		if !local && !isEnvProxy(tr.Proxy) {
			t.Errorf("off a local install the ping client's Proxy is not ProxyFromEnvironment")
		}
	}
}

func isEnvProxy(f func(*http.Request) (*url.URL, error)) bool {
	return f != nil && reflect.ValueOf(f).Pointer() == reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
}
