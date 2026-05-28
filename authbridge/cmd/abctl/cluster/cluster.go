// Package cluster talks to a Kubernetes cluster on behalf of abctl's
// picker UI. The production implementation shells out to `kubectl`;
// tests inject a stub command runner.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// sidecarContainerNames is the set of container names that mark a pod as
// an AuthBridge agent. Container names are the operator's stable contract;
// labels are not (the operator may evolve them).
var sidecarContainerNames = map[string]struct{}{
	"authbridge-proxy": {},
	"authbridge-envoy": {},
	"authbridge-lite":  {},
}

// Pod is the slice of pod state the picker UI cares about.
type Pod struct {
	Namespace string
	Name      string
	Phase     string
	Ready     bool      // true when every container in containerStatuses is ready
	StartedAt time.Time // status.startTime; zero if absent
}

// AgentNamespace is a namespace plus the AuthBridge-bearing pods inside it.
type AgentNamespace struct {
	Name string
	Pods []Pod
}

// kubectlPodList mirrors the JSON shape of `kubectl get pods -A -o json`.
// Only the fields we use are decoded.
type kubectlPodList struct {
	Items []struct {
		Metadata struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string    `json:"phase"`
			StartTime         time.Time `json:"startTime"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// parseAgentPods filters `kubectl get pods -A -o json` output to AuthBridge
// agents and groups them by namespace, sorted alphabetically.
func parseAgentPods(raw []byte) ([]AgentNamespace, error) {
	var list kubectlPodList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode kubectl pod list: %w", err)
	}
	byNs := map[string][]Pod{}
	for _, item := range list.Items {
		hasSidecar := false
		for _, c := range item.Spec.Containers {
			if _, ok := sidecarContainerNames[c.Name]; ok {
				hasSidecar = true
				break
			}
		}
		if !hasSidecar {
			continue
		}
		ready := len(item.Status.ContainerStatuses) > 0
		for _, cs := range item.Status.ContainerStatuses {
			if !cs.Ready {
				ready = false
				break
			}
		}
		byNs[item.Metadata.Namespace] = append(byNs[item.Metadata.Namespace], Pod{
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			Phase:     item.Status.Phase,
			Ready:     ready,
			StartedAt: item.Status.StartTime,
		})
	}
	out := make([]AgentNamespace, 0, len(byNs))
	for ns, pods := range byNs {
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		out = append(out, AgentNamespace{Name: ns, Pods: pods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runner abstracts a `kubectl <args>` invocation. Production uses os/exec;
// tests inject their own.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// defaultRunner shells out to the system `kubectl`.
func defaultRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		// exec.ExitError carries the stderr we want to surface. Drop the
		// "exit status N" prefix — the stderr already says what failed,
		// and no caller currently uses errors.As to match the ExitError.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}

// Lister enumerates AuthBridge-bearing pods in the cluster, grouped by
// namespace. Not safe for concurrent calls from multiple goroutines —
// the picker calls ListAgents from a single goroutine per pane entry.
type Lister interface {
	ListAgents(ctx context.Context) ([]AgentNamespace, error)
}

// NewLister returns a Lister that shells out to the system `kubectl`.
func NewLister() Lister { return &kubectlLister{run: defaultRunner} }

type kubectlLister struct{ run runner }

func (l *kubectlLister) ListAgents(ctx context.Context) ([]AgentNamespace, error) {
	out, err := l.run(ctx, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseAgentPods(out)
}
