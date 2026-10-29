package reverseproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/staticinject"
)

// MOCA AB1's shape, end to end: static-inject in the INBOUND pipeline, a
// PLAINTEXT front, and an HTTPS backend. The credential never crosses the
// plaintext hop — it is injected on the way out, to a TLS backend — so the
// request must be forwarded with the real key.
//
// This is a listener-level test on purpose. #1481's plaintext gate reads
// pctx.Scheme, which on the reverse proxy is requestScheme(r): the scheme the
// CLIENT used to reach Cortex, "http" for any non-TLS inbound listener. The
// backend's scheme comes from reverse_proxy_backend and is invisible from the
// plugin. So the gate, before it also checked Direction, refused every request
// through AB1 — 403 with zero upstream hits.
//
// The unit tests in core/plugins/staticinject could not have caught it: they
// build a pipeline.Context by hand, so the wrong-hop Scheme has to come from a
// real listener to show up. AB1 runs
// reverse_proxy_addr ":8080" in front of reverse_proxy_backend
// "https://api.anthropic.com" (moca deploy/knative/authbridge/ab1-deployment.yaml).
func TestStaticInject_InboundPlaintextFrontHTTPSBackend_StillInjects(t *testing.T) {
	var hits int64
	var sawKey string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		sawKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// AB1's static-inject config: key_by static, inject_header x-api-key.
	p := staticinject.New()
	cfg, err := json.Marshal(map[string]any{
		"source":        "mappings",
		"mappings":      map[string]string{"anthropic": "sk-real-secret"},
		"key_by":        "static",
		"key":           "anthropic",
		"inject_header": "x-api-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pl, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pl), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.proxy.Transport = backend.Client().Transport // trust the test CA

	// Plaintext front, as reverse_proxy_addr: ":8080".
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder bearer the workload presents; static-inject swaps it.
	req.Header.Set("Authorization", "Bearer placeholder-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("status = 403 — the inbound plaintext front was read as the credential's hop; AB1's every LLM call would be refused")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("backend hits = %d, want 1", n)
	}
	if sawKey != "sk-real-secret" {
		t.Errorf("backend saw x-api-key = %q, want the injected credential", sawKey)
	}
}
