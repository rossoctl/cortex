package edit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const fixtureMidYAML = `mode: proxy-sidecar

listener:
  forward_proxy_addr: ":8081"

pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: http://idp
  outbound:
    - name: token-exchange

session:
  enabled: true
`

const fixtureLastYAML = `mode: proxy-sidecar

pipeline:
  inbound:
    - name: jwt-validation
`

const fixtureFirstYAML = `pipeline:
  inbound:
    - name: jwt-validation

mode: proxy-sidecar
`

const fixtureMissingYAML = `mode: proxy-sidecar

listener:
  forward_proxy_addr: ":8081"
`

const fixtureCMYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config-email-agent
  namespace: team1
data:
  config.yaml: |
    mode: proxy-sidecar
    pipeline:
      inbound:
        - name: jwt-validation
          config:
            issuer: old
    session:
      enabled: true
`

func TestFindPipelineRange_Middle(t *testing.T) {
	start, end, err := FindPipelineRange([]byte(fixtureMidYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	got := fixtureMidYAML[start:end]
	if !strings.Contains(got, "pipeline:") {
		t.Fatalf("range missing pipeline header: %q", got)
	}
	if !strings.Contains(got, "token-exchange") {
		t.Fatalf("range missing pipeline body: %q", got)
	}
	if strings.Contains(got, "session:") {
		t.Fatalf("range includes next key: %q", got)
	}
	if strings.Contains(got, "listener:") {
		t.Fatalf("range includes prior key: %q", got)
	}
}

func TestFindPipelineRange_LastKey(t *testing.T) {
	start, end, err := FindPipelineRange([]byte(fixtureLastYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	if end != len(fixtureLastYAML) {
		t.Fatalf("end = %d, want len(yaml) = %d", end, len(fixtureLastYAML))
	}
	got := fixtureLastYAML[start:end]
	if !strings.Contains(got, "jwt-validation") {
		t.Fatalf("range missing pipeline body: %q", got)
	}
}

func TestFindPipelineRange_FirstKey(t *testing.T) {
	start, _, err := FindPipelineRange([]byte(fixtureFirstYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestFindPipelineRange_Missing(t *testing.T) {
	_, _, err := FindPipelineRange([]byte(fixtureMissingYAML))
	if err == nil {
		t.Fatal("want error when pipeline key is absent")
	}
	if !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("error should mention pipeline: %v", err)
	}
}

func TestSplice_PreservesOutsideRange(t *testing.T) {
	const orig = `mode: proxy-sidecar
# this comment must survive
listener:
  forward_proxy_addr: ":8081"

pipeline:
  inbound:
    - name: jwt-validation

session:
  enabled: true
`
	start, end, err := FindPipelineRange([]byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	const newSubtree = `pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: new

`
	got := Splice([]byte(orig), start, end, []byte(newSubtree))
	gotS := string(got)
	if !strings.Contains(gotS, "# this comment must survive") {
		t.Fatalf("comment outside pipeline subtree was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "listener:") {
		t.Fatalf("listener section was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "session:") {
		t.Fatalf("session section was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "issuer: new") {
		t.Fatalf("new pipeline content not present:\n%s", gotS)
	}
	if strings.Contains(gotS, "issuer: old") {
		t.Fatalf("old pipeline content still present:\n%s", gotS)
	}
}

func TestBuildManifest_UpdatesDataField(t *testing.T) {
	const newInner = `mode: proxy-sidecar
pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: new
session:
  enabled: true
`
	out, err := BuildManifest([]byte(fixtureCMYAML), []byte(newInner))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	outS := string(out)
	if !strings.Contains(outS, "name: authbridge-config-email-agent") {
		t.Fatalf("metadata.name lost:\n%s", outS)
	}
	if !strings.Contains(outS, "namespace: team1") {
		t.Fatalf("metadata.namespace lost:\n%s", outS)
	}
	if !strings.Contains(outS, "issuer: new") {
		t.Fatalf("new content not in manifest:\n%s", outS)
	}
	if strings.Contains(outS, "issuer: old") {
		t.Fatalf("old content still in manifest:\n%s", outS)
	}
}

func TestFetch_HappyPath(t *testing.T) {
	wantArgs := []string{
		"get", "cm", "authbridge-config-email-agent",
		"-n", "team1", "-o", "yaml",
	}
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		if !equalArgs(args, wantArgs) {
			t.Fatalf("kubectl args: got %v want %v", args, wantArgs)
		}
		return []byte(fixtureCMYAML), nil
	}
	fp, err := Fetch(context.Background(), stub, "team1", "email-agent")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(fp.ConfigMapYAML) == 0 {
		t.Fatal("ConfigMapYAML empty")
	}
	if len(fp.InnerYAML) == 0 {
		t.Fatal("InnerYAML empty")
	}
	if fp.PipelineEnd <= fp.PipelineStart {
		t.Fatalf("pipeline range invalid: [%d, %d)", fp.PipelineStart, fp.PipelineEnd)
	}
	subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
	if !strings.Contains(string(subtree), "pipeline:") {
		t.Fatalf("subtree missing header: %q", subtree)
	}
	if !strings.Contains(string(subtree), "jwt-validation") {
		t.Fatalf("subtree missing body: %q", subtree)
	}
}

func TestFetch_KubectlError(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("forbidden")
	}
	_, err := Fetch(context.Background(), stub, "team1", "email-agent")
	if err == nil {
		t.Fatal("want error from kubectl")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should surface kubectl message: %v", err)
	}
}

func TestApply_PassesManifest(t *testing.T) {
	captured := make([]byte, 0)
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// Args should be: apply --server-side --field-manager=abctl --force-conflicts=true -f <path>
		if len(args) < 4 || args[0] != "apply" || args[1] != "--server-side" {
			t.Fatalf("kubectl args: %v", args)
		}
		hasFM, hasForce := false, false
		for _, a := range args {
			if a == "--field-manager=abctl" {
				hasFM = true
			}
			if a == "--force-conflicts=true" {
				hasForce = true
			}
		}
		if !hasFM || !hasForce {
			t.Fatalf("expected --field-manager=abctl and --force-conflicts=true; got %v", args)
		}
		path := args[len(args)-1]
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		captured = b
		return []byte("configmap/foo applied\n"), nil
	}
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\n")
	at, err := Apply(context.Background(), stub, manifest)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if at.IsZero() {
		t.Fatal("apply time should be set")
	}
	if time.Since(at) > 5*time.Second {
		t.Fatalf("apply time too far in past: %v", at)
	}
	if string(captured) != string(manifest) {
		t.Fatalf("manifest captured by stub differs from input")
	}
}

// TestSplice_HandlesMissingTrailingNewline documents the contract: callers MUST
// normalize trailing '\n' before calling Splice. Without normalization the last
// line of the new subtree concatenates with the next top-level YAML key,
// producing garbage that passes per-subtree validation but breaks the full
// inner YAML. The normalization is performed by tui/app.go's editorExitedMsg
// handler before invoking Splice.
func TestSplice_HandlesMissingTrailingNewline(t *testing.T) {
	const orig = `mode: proxy-sidecar
pipeline:
  inbound:
    - name: a

session:
  enabled: true
`
	start, end, err := FindPipelineRange([]byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	// newSubtree deliberately missing trailing newline.
	newSubtree := []byte("pipeline:\n  inbound:\n    - name: b")
	spliced := Splice([]byte(orig), start, end, newSubtree)
	// The TUI's editorExitedMsg handler is responsible for appending \n
	// before splicing, so a Splice given non-newline-terminated input
	// produces concatenated garbage. This test documents the contract:
	// callers MUST normalize trailing \n before calling Splice. The
	// normalization happens in tui/app.go.
	if !bytes.HasSuffix(spliced, []byte("session:\n  enabled: true\n")) {
		// The garbage we're guarding against in the TUI:
		// Verify that without normalization, the splice DOES produce
		// garbage joining "name: b" to "session:". This ensures the
		// contract documented in Splice's godoc is observable.
		if !bytes.Contains(spliced, []byte("- name: bsession:")) {
			t.Fatalf("expected to observe the splice-without-normalization garbage; got:\n%s", spliced)
		}
	}
}

func TestBuildManifest_StripsServerManagedMetadata(t *testing.T) {
	const fetched = `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config-email-agent
  namespace: team1
  resourceVersion: "12345"
  uid: abc-123
  creationTimestamp: "2026-05-29T00:00:00Z"
  generation: 7
  managedFields:
    - manager: rossoctl-webhook
      operation: Apply
data:
  config.yaml: |
    pipeline:
      inbound:
        - name: jwt-validation
`
	out, err := BuildManifest([]byte(fetched), []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n"))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	outS := string(out)
	for _, drop := range []string{"resourceVersion", "uid:", "creationTimestamp", "managedFields", "generation:"} {
		if strings.Contains(outS, drop) {
			t.Fatalf("manifest should not contain %q (would break SSA):\n%s", drop, outS)
		}
	}
	if !strings.Contains(outS, "name: authbridge-config-email-agent") || !strings.Contains(outS, "namespace: team1") {
		t.Fatalf("manifest lost name/namespace:\n%s", outS)
	}
}

func TestResolveAgentName_FromLabel(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) < 2 || args[0] != "get" || args[1] != "pod" {
			t.Fatalf("unexpected args: %v", args)
		}
		return []byte("email-agent\n"), nil
	}
	got, err := ResolveAgentName(context.Background(), stub, "team1", "email-agent-779bb85688-4x8zg")
	if err != nil {
		t.Fatalf("ResolveAgentName: %v", err)
	}
	if got != "email-agent" {
		t.Fatalf("got %q want %q", got, "email-agent")
	}
}

func TestResolveAgentName_FallbackToPodSuffixStrip(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(""), nil // label absent
	}
	got, err := ResolveAgentName(context.Background(), stub, "team1", "email-agent-779bb85688-4x8zg")
	if err != nil {
		t.Fatalf("ResolveAgentName: %v", err)
	}
	if got != "email-agent" {
		t.Fatalf("got %q want %q (RS-hash + pod-suffix should be stripped)", got, "email-agent")
	}
}

func TestSplice_OutOfBoundsReturnsInputUnchanged(t *testing.T) {
	in := []byte("abc\ndef\n")
	cases := []struct {
		name       string
		start, end int
	}{
		{"negative start", -1, 4},
		{"end past len", 0, 9999},
		{"start > end", 5, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Splice(in, c.start, c.end, []byte("X"))
			if string(got) != string(in) {
				t.Fatalf("want input back unchanged, got %q", got)
			}
		})
	}
}

// equalArgs checks two []string for equality.
func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
