// Package edit implements abctl's in-place pipeline editor. The flow is:
// fetch the agent's ConfigMap via kubectl, locate the pipeline: subtree,
// open just that subtree in the user's $EDITOR, splice the edit back into
// the original ConfigMap manifest, kubectl apply --server-side, then poll
// /reload/status until the framework reloads.
//
// All kubectl interaction goes through the Runner injection seam so tests
// can stub it out.
package edit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FindPipelineRange returns the byte offsets [start, end) in innerYAML
// that span the "pipeline:" subtree, including the "pipeline:" key line
// itself but not any following top-level keys. Used by the editor to
// extract just the pipeline subtree for the user, and by Splice to
// replace it with the user's edit.
//
// Returns an error if innerYAML is not valid YAML or if no top-level
// "pipeline" key exists.
func FindPipelineRange(innerYAML []byte) (start, end int, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(innerYAML, &root); err != nil {
		return 0, 0, fmt.Errorf("parse runtime YAML: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return 0, 0, fmt.Errorf("runtime YAML is not a document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return 0, 0, fmt.Errorf("runtime YAML root is not a mapping")
	}

	// Children of a MappingNode alternate key, value, key, value, ...
	// Find the index of the "pipeline" key, capture its line, and find
	// the next sibling's line (or end-of-document if it's the last key).
	pipelineKeyIdx := -1
	for i := 0; i < len(doc.Content); i += 2 {
		k := doc.Content[i]
		if k.Value == "pipeline" {
			pipelineKeyIdx = i
			break
		}
	}
	if pipelineKeyIdx == -1 {
		return 0, 0, fmt.Errorf("no top-level pipeline key in runtime YAML")
	}

	pipelineKeyLine := doc.Content[pipelineKeyIdx].Line // 1-indexed
	var nextKeyLine int                                 // 1-indexed; 0 if pipeline is last
	if pipelineKeyIdx+2 < len(doc.Content) {
		nextKeyLine = doc.Content[pipelineKeyIdx+2].Line
	}

	// Map line numbers to byte offsets. yaml.v3 Line is 1-indexed.
	lineStarts := []int{0} // lineStarts[i] = byte offset where line i+1 starts
	for i, b := range innerYAML {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	if pipelineKeyLine < 1 || pipelineKeyLine > len(lineStarts) {
		return 0, 0, fmt.Errorf("pipeline key line %d out of range", pipelineKeyLine)
	}
	start = lineStarts[pipelineKeyLine-1]

	if nextKeyLine == 0 {
		end = len(innerYAML)
	} else {
		if nextKeyLine < 1 || nextKeyLine > len(lineStarts) {
			return 0, 0, fmt.Errorf("next-key line %d out of range", nextKeyLine)
		}
		end = lineStarts[nextKeyLine-1]
	}
	return start, end, nil
}

// Splice replaces the byte range [start, end) of innerYAML with newSubtree
// and returns the result. Used to apply the user's edit to just the pipeline
// subtree, leaving everything outside it byte-for-byte unchanged. Comments,
// blank lines, and field ordering outside the pipeline subtree all survive.
func Splice(innerYAML []byte, start, end int, newSubtree []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(innerYAML) - (end - start) + len(newSubtree))
	b.Write(innerYAML[:start])
	b.Write(newSubtree)
	b.Write(innerYAML[end:])
	return b.Bytes()
}

// BuildManifest takes the original ConfigMap YAML manifest (as returned by
// kubectl get cm -o yaml) and a new inner runtime YAML (the contents that
// belong in data.config.yaml). Returns a manifest ready for kubectl apply.
//
// The manifest passes through yaml.v3 so the outer structure (apiVersion,
// kind, metadata, etc.) is preserved. Only data.config.yaml is replaced.
// Comments inside the inner runtime YAML survive because we set the
// data.config.yaml value to a literal block (|) string carrying newInner
// verbatim.
func BuildManifest(origCMYAML, newInner []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(origCMYAML, &root); err != nil {
		return nil, fmt.Errorf("parse ConfigMap manifest: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("ConfigMap manifest is not a document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ConfigMap manifest root is not a mapping")
	}

	// Find data → config.yaml.
	var dataNode *yaml.Node
	for i := 0; i < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "data" {
			dataNode = doc.Content[i+1]
			break
		}
	}
	if dataNode == nil || dataNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ConfigMap has no data: mapping")
	}
	var configValueNode *yaml.Node
	for i := 0; i < len(dataNode.Content); i += 2 {
		if dataNode.Content[i].Value == "config.yaml" {
			configValueNode = dataNode.Content[i+1]
			break
		}
	}
	if configValueNode == nil {
		return nil, fmt.Errorf("ConfigMap data has no config.yaml key")
	}

	// Set the value to a literal-block scalar carrying newInner.
	configValueNode.Kind = yaml.ScalarNode
	configValueNode.Tag = "!!str"
	configValueNode.Style = yaml.LiteralStyle
	configValueNode.Value = string(newInner)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("emit ConfigMap manifest: %w", err)
	}
	return out, nil
}

// Runner abstracts a `kubectl <args>` invocation. Production uses os/exec;
// tests inject their own. Mirrors the Runner pattern in cmd/abctl/cluster.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// DefaultRunner shells out to the system `kubectl`.
func DefaultRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}

// FetchedPipeline is what Fetch returns: the full ConfigMap manifest, the
// inner runtime YAML extracted from data.config.yaml, and the byte range
// of the pipeline subtree within the inner YAML.
type FetchedPipeline struct {
	ConfigMapYAML []byte // raw kubectl get cm -o yaml output
	InnerYAML     []byte // value of data.config.yaml
	PipelineStart int    // byte offset in InnerYAML where pipeline: begins
	PipelineEnd   int    // byte offset where the subtree ends
}

// ResolveAgentName looks up the agent name for a pod via its
// app.kubernetes.io/name label, which matches the per-agent ConfigMap
// suffix (authbridge-config-<agent>). Falls back to stripping the last
// two dash-separated segments (the ReplicaSet hash + pod suffix) when
// the label is absent.
func ResolveAgentName(ctx context.Context, run Runner, namespace, pod string) (string, error) {
	out, err := run(ctx, "get", "pod", pod, "-n", namespace,
		"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/name}")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name != "" {
		return name, nil
	}
	parts := strings.Split(pod, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[:len(parts)-2], "-"), nil
	}
	return pod, nil
}

// Fetch reads the per-agent ConfigMap (authbridge-config-<agent>), extracts
// the inner runtime YAML from data.config.yaml, and locates the pipeline
// subtree's byte range. Returns an error if the ConfigMap doesn't exist,
// has no data.config.yaml, or has no top-level pipeline: key.
func Fetch(ctx context.Context, run Runner, namespace, agent string) (*FetchedPipeline, error) {
	cmName := "authbridge-config-" + agent
	cmBytes, err := run(ctx, "get", "cm", cmName, "-n", namespace, "-o", "yaml")
	if err != nil {
		return nil, err
	}
	inner, err := extractInnerYAML(cmBytes)
	if err != nil {
		return nil, err
	}
	start, end, err := FindPipelineRange(inner)
	if err != nil {
		return nil, err
	}
	return &FetchedPipeline{
		ConfigMapYAML: cmBytes,
		InnerYAML:     inner,
		PipelineStart: start,
		PipelineEnd:   end,
	}, nil
}

// extractInnerYAML pulls data.config.yaml out of an outer ConfigMap manifest.
func extractInnerYAML(cmYAML []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(cmYAML, &root); err != nil {
		return nil, fmt.Errorf("parse ConfigMap manifest: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("ConfigMap manifest is not a document")
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "data" {
			dataNode := doc.Content[i+1]
			for j := 0; j+1 < len(dataNode.Content); j += 2 {
				if dataNode.Content[j].Value == "config.yaml" {
					return []byte(dataNode.Content[j+1].Value), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("ConfigMap data has no config.yaml key")
}

// Apply writes manifest to a tempfile and runs kubectl apply --server-side
// with --force-conflicts=true and a dedicated abctl field-manager. The
// kagenti-operator's webhook owns data.config.yaml on initial creation;
// the user has explicitly confirmed this edit by pressing "y" at the
// diff prompt, so taking field-manager ownership is the intended outcome.
//
// Returns the wall-clock time at which the apply call started; the caller
// uses this to compare against /reload/status's last_success_unix to know
// whether the framework has picked up the change yet.
func Apply(ctx context.Context, run Runner, manifest []byte) (time.Time, error) {
	tmp, err := os.CreateTemp("", "abctl-cm-*.yaml")
	if err != nil {
		return time.Time{}, fmt.Errorf("create temp manifest: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(manifest); err != nil {
		_ = tmp.Close()
		return time.Time{}, fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close temp manifest: %w", err)
	}
	applyTime := time.Now()
	if _, err := run(ctx, "apply", "--server-side",
		"--field-manager=abctl",
		"--force-conflicts=true",
		"-f", tmp.Name()); err != nil {
		return time.Time{}, err
	}
	return applyTime, nil
}
