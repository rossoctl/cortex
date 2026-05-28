//go:build e2e

package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2ESmoke runs against the active kubectl context. Set up by
// running the IBAC demo (make demo-ibac) before invoking:
//
//   go test -tags=e2e ./cluster/ -run TestE2ESmoke -v
//
// The test fails clearly if no AuthBridge agent is found.
func TestE2ESmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lister := NewLister()
	groups, err := lister.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("no AuthBridge namespaces found; run `make demo-ibac` first")
	}
	var ns, pod string
	for _, g := range groups {
		for _, p := range g.Pods {
			if p.Ready {
				ns, pod = g.Name, p.Name
				break
			}
		}
		if pod != "" {
			break
		}
	}
	if pod == "" {
		t.Fatal("no Ready AuthBridge pod found")
	}

	pf, err := NewPortForwarder().Start(ctx, ns, pod)
	if err != nil {
		t.Fatalf("Start port-forward: %v", err)
	}
	defer pf.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", pf.Endpoint()+"/healthz", nil)
	if err != nil {
		t.Fatalf("build /healthz request: %v", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status: %d", resp.StatusCode)
	}
	if !strings.HasPrefix(pf.Endpoint(), "http://127.0.0.1:") {
		t.Fatalf("unexpected endpoint shape: %s", pf.Endpoint())
	}
}
