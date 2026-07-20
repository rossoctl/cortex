package reverseproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// rewritePlugin sets a new Authorization on the inbound context, simulating
// jwt-validation mint mode.
type rewritePlugin struct{}

func (rewritePlugin) Name() string                              { return "rewrite" }
func (rewritePlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (rewritePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer abph_minted")
	return pipeline.Action{Type: pipeline.Continue}
}
func (rewritePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestInboundPropagation_RewrittenAuthReachesBackend(t *testing.T) {
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{rewritePlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/work", nil)
	req.Header.Set("Authorization", "Bearer real-user-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	if seen != "Bearer abph_minted" {
		t.Fatalf("backend saw Authorization=%q, want Bearer abph_minted", seen)
	}
}
