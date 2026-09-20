package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePipelineServer answers GET /v1/pipeline with body and 404s everything else, so a
// test states exactly what the proxy reported and nothing else can satisfy the command
// by accident.
//
// A server rather than an injected decoder, for the reason fakeUsageServer gives: the
// command's job includes reaching the endpoint and turning a failure into a recovery
// hint, and a stubbed transport would test everything except that.
func fakePipelineServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

// twoChainPipeline deliberately mixes a plugin that declares a description with one that
// does not, and gives the description-less one a config: the row layout and the config
// block both change shape on that field, so a fixture where every plugin has one would
// leave half the rendering untested.
const twoChainPipeline = `{"inbound":[{"name":"jwt-validation","direction":"inbound",` +
	`"position":1,"readsBody":false,"description":"Inbound JWT validation against JWKS.",` +
	`"config":{"issuer":"http://idp.example/realms/r"}}],` +
	`"outbound":[{"name":"inference-parser","direction":"outbound","position":1,"readsBody":true,` +
	`"description":"Parses LLM completions into pctx.Extensions.Inference."},` +
	`{"name":"token-exchange","direction":"outbound","position":2,"readsBody":false,` +
	`"requires":["jwt-validation"],"config":{"default_policy":"passthrough"}}]}`

// TestRunPipelineGet_PrintsBothChainsAndTheDivider covers the shape of the human output:
// every plugin from both directions, its body flag, and the application between the
// chains — which is what makes the ordering mean anything.
func TestRunPipelineGet_PrintsBothChainsAndTheDivider(t *testing.T) {
	srv := fakePipelineServer(t, twoChainPipeline)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"jwt-validation", "inference-parser", "token-exchange",
		"inbound", "outbound",
		"(app)",        // the divider between the chains
		"yes",          // inference-parser reads the body
		"issuer",       // the config, indented under its plugin
		"DESCRIPTION",  // the column header
		"against JWKS", // and a plugin's own description
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The divider has to sit BETWEEN the chains, not merely be present: a command that
	// printed it first or last would pass a contains-check while telling the operator
	// the wrong thing about what runs before the app.
	app := strings.Index(got, "(app)")
	if in, outb := strings.Index(got, "jwt-validation"), strings.Index(got, "token-exchange"); !(in < app && app < outb) {
		t.Errorf("divider at %d is not between inbound (%d) and outbound (%d):\n%s", app, in, outb, got)
	}
}

// TestRunPipelineGet_JSONIsTheEndpointsOwnShape pins that --json speaks /v1/pipeline's
// vocabulary rather than one of its own. A script reading this command and a script
// reading the endpoint must not have to learn two names for the same field.
func TestRunPipelineGet_JSONIsTheEndpointsOwnShape(t *testing.T) {
	srv := fakePipelineServer(t, twoChainPipeline)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Inbound []struct {
			Name      string          `json:"name"`
			Position  int             `json:"position"`
			ReadsBody bool            `json:"readsBody"`
			Config    json.RawMessage `json:"config"`
		} `json:"inbound"`
		Outbound []struct {
			Name     string   `json:"name"`
			Requires []string `json:"requires"`
		} `json:"outbound"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(decoded.Inbound) != 1 || decoded.Inbound[0].Name != "jwt-validation" {
		t.Errorf("inbound chain did not survive the round trip: %+v", decoded.Inbound)
	}
	if len(decoded.Outbound) != 2 || decoded.Outbound[1].Name != "token-exchange" {
		t.Fatalf("outbound chain did not survive the round trip: %+v", decoded.Outbound)
	}
	// requires is what a caller needs to compute the dependency status this command's
	// table deliberately omits, so its presence is part of the contract.
	if len(decoded.Outbound[1].Requires) != 1 {
		t.Errorf("requires did not reach --json: %+v", decoded.Outbound[1])
	}
	// And the config passes through as an object rather than a re-encoded string.
	if !strings.HasPrefix(strings.TrimSpace(string(decoded.Inbound[0].Config)), "{") {
		t.Errorf("config is not a JSON object: %s", decoded.Inbound[0].Config)
	}
}

// TestRunPipelineGet_EmptyPipelineSaysSo — a proxy with no plugins is a legal
// configuration, and a blank table cannot be told from a failed decode.
func TestRunPipelineGet_EmptyPipelineSaysSo(t *testing.T) {
	srv := fakePipelineServer(t, `{"inbound":[],"outbound":[]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "no plugins") {
		t.Errorf("an empty pipeline printed nothing that says so:\n%q", got)
	}
}

// TestRunPipeline_RequiresAnAction keeps `abctl pipeline` from silently doing something.
// The usage block goes to stderr with exit 2, while an explicit --help is a successful
// request answered on stdout — the split `abctl tools` and `abctl service` already use,
// and the reason `abctl pipeline get --json` can be piped.
func TestRunPipeline_RequiresAnAction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantOn   string // "stdout" or "stderr"
	}{
		{"no action", nil, 2, "stderr"},
		{"unknown action", []string{"list"}, 2, "stderr"},
		{"explicit help", []string{"--help"}, 0, "stdout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if code := runPipeline(tc.args, &out, &errOut); code != tc.wantCode {
				t.Errorf("exit = %d, want %d", code, tc.wantCode)
			}
			got, other := out.String(), errOut.String()
			if tc.wantOn == "stderr" {
				got, other = errOut.String(), out.String()
			}
			if !strings.Contains(got, "abctl pipeline get") {
				t.Errorf("usage not on %s:\n%q", tc.wantOn, got)
			}
			if other != "" {
				t.Errorf("unexpected output on the other stream:\n%q", other)
			}
		})
	}
}

// TestRunPipelineGet_UnreachableProxySaysWhatToRun — the error path is most of what this
// command does on a laptop where Cortex is not running, and a bare dial error tells the
// user nothing about the next step.
func TestRunPipelineGet_UnreachableProxySaysWhatToRun(t *testing.T) {
	srv := fakePipelineServer(t, twoChainPipeline)
	url := srv.URL
	srv.Close() // nothing is listening now

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", url}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got := errOut.String(); !strings.Contains(got, "abctl service status") {
		t.Errorf("stderr does not name the next command:\n%s", got)
	}
	if out.String() != "" {
		t.Errorf("wrote to stdout on failure:\n%q", out.String())
	}
}

// TestRunPipelineGet_MissingEndpointBlamesTheEndpointNotTheProxy — a 404 from a
// reachable proxy means its session API was built without /v1/pipeline, which is a
// different problem from a proxy that is down. Sending this user to `service status`
// points at the one thing that is working.
func TestRunPipelineGet_MissingEndpointBlamesTheEndpointNotTheProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got := errOut.String()
	if !strings.Contains(got, "/v1/pipeline") {
		t.Errorf("stderr does not name the missing endpoint:\n%s", got)
	}
	if strings.Contains(got, "abctl service status") {
		t.Errorf("a reachable proxy was blamed on Cortex not running:\n%s", got)
	}
}

// TestRunPipelineGet_DescriptionIsOptionalAndCostsNothingWhenAbsent covers the two things
// that broke while the DESCRIPTION column was being added, both invisible on a terminal.
//
// A description is omitempty on the wire, so the row layout has to change shape on it. The
// first version padded BODY to a fixed width unconditionally, which left a run of trailing
// spaces on every plugin declaring no description — nothing to see on screen, but it lands
// in a redirected file and in a diff, and no hook in this repo inspects a program's output
// for it. The second version fixed that with an early return, which skipped the config
// block underneath for exactly those plugins.
func TestRunPipelineGet_DescriptionIsOptionalAndCostsNothingWhenAbsent(t *testing.T) {
	// One plugin, no description, with a config — the combination the early return dropped.
	srv := fakePipelineServer(t, `{"inbound":[],"outbound":[{"name":"token-exchange",`+
		`"direction":"outbound","position":1,"readsBody":false,`+
		`"config":{"default_policy":"passthrough"}}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	// The config still renders when the description above it is absent.
	if !strings.Contains(got, "default_policy") {
		t.Errorf("a plugin with config but no description lost its config:\n%s", got)
	}
	// And no line ends in whitespace.
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d ends in whitespace: %q", i, line)
		}
	}
}
