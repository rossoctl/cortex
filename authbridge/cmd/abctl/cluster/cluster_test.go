package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fixturePodsJSON is a stripped-down `kubectl get pods -A -o json` payload
// covering: a matching pod (authbridge-proxy sidecar), a non-matching pod
// (no authbridge container), and a pod in a second namespace with a
// different sidecar variant (authbridge-envoy).
const fixturePodsJSON = `{
  "items": [
    {
      "metadata": {"namespace": "team1", "name": "weather-agent-1"},
      "spec": {"containers": [{"name": "agent"}, {"name": "authbridge-proxy"}]},
      "status": {"phase": "Running",
                 "containerStatuses": [{"ready": true}, {"ready": true}]}
    },
    {
      "metadata": {"namespace": "team1", "name": "unrelated-1"},
      "spec": {"containers": [{"name": "app"}]},
      "status": {"phase": "Running",
                 "containerStatuses": [{"ready": true}]}
    },
    {
      "metadata": {"namespace": "team2", "name": "billing-agent-1"},
      "spec": {"containers": [{"name": "agent"}, {"name": "authbridge-envoy"}]},
      "status": {"phase": "Pending",
                 "containerStatuses": [{"ready": false}, {"ready": false}]}
    }
  ]
}`

func TestParseAgentPods(t *testing.T) {
	got, err := parseAgentPods([]byte(fixturePodsJSON))
	if err != nil {
		t.Fatalf("parseAgentPods: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 namespaces, got %d", len(got))
	}
	// Namespaces must be sorted alphabetically.
	if got[0].Name != "team1" || got[1].Name != "team2" {
		t.Fatalf("namespace order wrong: %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[0].Pods) != 1 {
		t.Fatalf("team1: want 1 pod, got %d", len(got[0].Pods))
	}
	if got[0].Pods[0].Name != "weather-agent-1" {
		t.Fatalf("team1 pod name: got %q", got[0].Pods[0].Name)
	}
	if !got[0].Pods[0].Ready {
		t.Fatalf("team1 pod should be Ready")
	}
	if got[1].Pods[0].Ready {
		t.Fatalf("team2 pod should NOT be Ready")
	}
}

func TestParseAgentPodsRejectsBadJSON(t *testing.T) {
	_, err := parseAgentPods([]byte("not json"))
	if err == nil {
		t.Fatal("want error on bad JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error should mention decode failure, got %v", err)
	}
}

func TestKubectlListerListAgents(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// Verify the args we pass kubectl.
		want := []string{"get", "pods", "-A", "-o", "json"}
		if len(args) != len(want) {
			t.Fatalf("kubectl args: got %v want %v", args, want)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("kubectl args[%d]: got %q want %q", i, args[i], want[i])
			}
		}
		return []byte(fixturePodsJSON), nil
	}
	l := &kubectlLister{run: stub}
	got, err := l.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 namespaces, got %d", len(got))
	}
}

func TestKubectlListerSurfacesRunnerError(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("kubectl: forbidden")
	}
	l := &kubectlLister{run: stub}
	_, err := l.ListAgents(context.Background())
	if err == nil {
		t.Fatal("want error from runner to surface")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should include runner output, got %v", err)
	}
}
