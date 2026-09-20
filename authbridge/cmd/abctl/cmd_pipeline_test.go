package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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

// bodyColumn returns the display column at which a row's BODY cell starts, or -1 when the
// line carries no recognisable row.
//
// Display columns via lipgloss.Width, not a byte or rune index: the property under test is
// where a cell LANDS on a terminal, and a byte offset says something different the moment a
// name is not ASCII — which is the whole bug this pins.
func bodyColumn(line string) int {
	// Anchored on the FIELD, not on a substring. Searching for "  no" found the first
	// occurrence anywhere on the line, so a plugin named "no" made this report the name's
	// column instead of BODY's — 17 against the real 30 — and then compared two wrong
	// columns while still passing. That weakens the one test protecting alignment, which is
	// the property two shipped bugs already slipped past.
	//
	// This command writes no escape sequences — see writePipelineTable's plain-text comment
	// — so there is nothing to strip before measuring.
	//
	// The LAST such cell, not the first. A plugin may legitimately be named "no", and then
	// a first-match scan reports the name's column — 17 against BODY's real 30 — and
	// compares two wrong columns while still passing. BODY is the final cell on a row that
	// carries no description, and on a row that has one the description follows, so taking
	// the last "no"/"yes" cell is right in both shapes: a description equal to exactly "no"
	// with nothing else on it is not a case worth contorting for.
	//
	// Cells are runs of non-space, walked with the gaps between them, rather than split on a
	// fixed two-space separator: a padded cell is followed by MORE than two spaces, which a
	// fixed split turns into empty fields.
	col, i, found := 0, 0, -1
	r := []rune(line)
	for i < len(r) {
		for i < len(r) && r[i] == ' ' { // the gap before this cell
			col++
			i++
		}
		start := i
		for i < len(r) && r[i] != ' ' { // the cell itself
			i++
		}
		if cell := string(r[start:i]); cell == "no" || cell == "yes" {
			found = col
		}
		col += lipgloss.Width(string(r[start:i]))
	}
	return found
}

// TestRunPipelineGet_ColumnsAlignAcrossRows is the assertion the first version of this suite
// did not have, and its absence is why two alignment bugs shipped past it.
//
// Every other test here is strings.Contains or strings.Index, so replacing both format
// strings with completely unpadded versions left all seven passing — the command's whole
// design argument is that the columns line up, and nothing checked it. This compares the
// display column of BODY across rows, which is the one thing that must hold whatever the
// names are.
//
// The CJK case is the point. Padding with "%-*s" counts BYTES while the terminal counts
// CELLS, so a three-byte two-column rune stops the padding early and shifts every column to
// its right — measured at 8 columns of drift before the fix. An ASCII-only fixture cannot
// see it, which is why one is not used here.
func TestRunPipelineGet_ColumnsAlignAcrossRows(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"ascii names", `{"inbound":[{"name":"jwt-validation","direction":"inbound","position":1,"readsBody":false},` +
			`{"name":"a2a-parser","direction":"inbound","position":2,"readsBody":true}],"outbound":[]}`},
		// The wide name must be SHORTER in display cells than the longest name, so the
		// column genuinely needs padding. A wide name that is also the longest needs none,
		// and then a byte-counting pad computes a negative width, adds nothing, and lands
		// on the right column by accident — which a len()-vs-Width mutation survives.
		// "日本語" is 3 cells wide and 9 bytes long against a 14-cell column.
		{"a wide-character name that needs padding", `{"inbound":[{"name":"日本語","direction":"inbound",` +
			`"position":1,"readsBody":false},{"name":"jwt-validation","direction":"inbound","position":2,` +
			`"readsBody":true}],"outbound":[]}`},
		// DIRECTION is server-controlled too, and was padded with "%-9s" — the same
		// byte-counting verb, 2 columns of drift on a wide value.
		{"a wide-character direction", `{"inbound":[{"name":"aa","direction":"日本","position":1,` +
			`"readsBody":false},{"name":"bb","direction":"inbound","position":2,"readsBody":true}],` +
			`"outbound":[]}`},
		// A plugin named exactly "no" is what broke the bodyColumn helper: a first-match
		// substring scan reported the NAME's column and then compared two wrong ones.
		{"a plugin named no", `{"inbound":[{"name":"no","direction":"inbound","position":1,` +
			`"readsBody":false},{"name":"longer-name","direction":"inbound","position":2,` +
			`"readsBody":true}],"outbound":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakePipelineServer(t, tc.body)
			defer srv.Close()

			var out, errOut strings.Builder
			if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
			}
			var cols []int
			for _, line := range strings.Split(out.String(), "\n") {
				if c := bodyColumn(line); c >= 0 {
					cols = append(cols, c)
				}
			}
			if len(cols) < 2 {
				t.Fatalf("found %d BODY cells, need at least 2 to compare:\n%s", len(cols), out.String())
			}
			for _, c := range cols[1:] {
				if c != cols[0] {
					t.Errorf("BODY starts at columns %v — the rows do not line up:\n%s", cols, out.String())
					break
				}
			}
		})
	}
}

// TestRunPipelineGet_DividerOnlyBetweenTwoChains — the divider asserts that plugins run
// before and after the application, so it must not appear when one side is empty.
//
// Not hypothetical: demos/context-guru/k8s/authbridge-config.yaml in this repo ships
// `inbound.plugins: []` with an outbound chain, and against that the divider printed as the
// FIRST row of the table.
func TestRunPipelineGet_DividerOnlyBetweenTwoChains(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantDivide bool
	}{
		{"outbound only", `{"inbound":[],"outbound":[{"name":"inference-parser","direction":"outbound",` +
			`"position":1,"readsBody":true}]}`, false},
		{"inbound only", `{"inbound":[{"name":"jwt-validation","direction":"inbound","position":1,` +
			`"readsBody":false}],"outbound":[]}`, false},
		{"both chains", twoChainPipeline, true},
		// A direction wider than "DIRECTION" widens that column, which is what the
		// divider's indent has to follow. The fixtures above cannot catch a hardcoded
		// indent, because 9-cell directions make the old literal accidentally right.
		{"both chains, wide direction", `{"inbound":[{"name":"aa","direction":"a-very-long-direction",` +
			`"position":1,"readsBody":false}],"outbound":[{"name":"bb","direction":"outbound",` +
			`"position":2,"readsBody":true}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakePipelineServer(t, tc.body)
			defer srv.Close()

			var out, errOut strings.Builder
			if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
			}
			if got := strings.Contains(out.String(), "(app)"); got != tc.wantDivide {
				t.Errorf("divider present = %v, want %v:\n%s", got, tc.wantDivide, out.String())
			}
			if !tc.wantDivide {
				return
			}
			// And it lines up under PLUGIN. Neither divider case checked the indent, which
			// was a literal 17 — correct only while DIRECTION stayed the widest value in
			// its own column, and Direction comes from the server.
			var dividerCol, pluginCol int
			for _, line := range strings.Split(out.String(), "\n") {
				if i := strings.Index(line, "──"); i >= 0 {
					dividerCol = lipgloss.Width(line[:i])
				}
				if i := strings.Index(line, "PLUGIN"); i >= 0 {
					pluginCol = lipgloss.Width(line[:i])
				}
			}
			if dividerCol != pluginCol {
				t.Errorf("divider starts at column %d, PLUGIN at %d:\n%s",
					dividerCol, pluginCol, out.String())
			}
		})
	}
}

// TestRunPipelineGet_RejectsAPositionalArgument — flag.Parse stops at the first non-flag
// argument, so a stray word used to swallow every flag after it: the command fell back to
// the local proxy, answered about a different one than the operator named, printed a human
// table for a caller that asked for JSON, and exited 0.
func TestRunPipelineGet_RejectsAPositionalArgument(t *testing.T) {
	var out, errOut strings.Builder
	// An endpoint that cannot be reached, so a wrong exit code cannot come from a
	// successful fetch: the only way to exit 2 here is the argument check.
	code := runPipeline([]string{"get", "typo", "--json", "--endpoint", "http://127.0.0.1:1"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if got := errOut.String(); !strings.Contains(got, "typo") {
		t.Errorf("stderr does not name the offending argument:\n%s", got)
	}
	if out.String() != "" {
		t.Errorf("wrote to stdout despite refusing:\n%q", out.String())
	}
}

// TestRunPipelineGet_AbsentChainIsAnEmptyArrayNotNull — the server states [] for an empty
// chain and pins it ("Empty slices, not null — the UI expects []"). This command re-encodes
// a decoded struct, so a response that OMITS a key would marshal back as null and break the
// shape --json promises. A compliant proxy never omits one; an older one does.
func TestRunPipelineGet_AbsentChainIsAnEmptyArrayNotNull(t *testing.T) {
	srv := fakePipelineServer(t, `{"outbound":[{"name":"x","direction":"outbound","position":1,"readsBody":false}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "null") {
		t.Errorf("an omitted chain became null rather than []:\n%s", out.String())
	}
}

// TestRunPipelineGet_AHostileNameCannotSplitTheRow — Name arrives in the same
// unauthenticated response as Description and lands in the same Fprintf, so sanitising only
// the description left the row splittable: a newline in a name produced a fragment reading
// "BODY-FAKE  no", which looks like a legitimate row rather than like damage. A name also
// feeds the column-width scan, so one hostile value perturbs every other row.
func TestRunPipelineGet_AHostileNameCannotSplitTheRow(t *testing.T) {
	// The newline is a JSON \n escape, not a raw byte: a raw newline inside a JSON string
	// is invalid and the decoder rejects the response before this command renders anything,
	// which is a different (and already handled) failure from the one under test.
	srv := fakePipelineServer(t, `{"inbound":[],"outbound":[{"name":"evil\nBODY-FAKE",`+
		`"direction":"outbound","position":1,"readsBody":false},`+
		`{"name":"ok","direction":"outbound","position":2,"readsBody":true}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// Header plus two rows: a split row would make four.
	if n := len(strings.Split(strings.TrimRight(got, "\n"), "\n")); n != 3 {
		t.Errorf("output is %d lines, want 3 — a name split its row:\n%q", n, got)
	}
	for _, r := range got {
		if r != '\n' && (r == 0x7f || r < 0x20) {
			t.Errorf("a control character reached the output: %q", got)
			break
		}
	}
}

// TestRunPipelineGet_ADescriptionCannotSplitTheRow — descriptions arrive over an
// unauthenticated endpoint and are interpolated into a table row. A newline splits the row
// and leaves an unindented continuation carrying trailing whitespace, which is exactly the
// property the unpadded-BODY branch exists to hold; an ESC sequence reaches the terminal.
func TestRunPipelineGet_ADescriptionCannotSplitTheRow(t *testing.T) {
	// "first\nsecond   \x1b[31mred" — built with explicit escapes so the fixture stays
	// readable and this file stays free of literal control bytes.
	srv := fakePipelineServer(t, `{"inbound":[],"outbound":[{"name":"x","direction":"outbound",`+
		`"position":1,"readsBody":false,"description":"first\nsecond   \u001b[31mred"}]}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPipeline([]string{"get", "--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// One header line plus one row: a split row would make three.
	if n := len(strings.Split(strings.TrimRight(got, "\n"), "\n")); n != 2 {
		t.Errorf("output is %d lines, want 2 — the description split the row:\n%q", n, got)
	}
	for _, r := range got {
		if r != '\n' && (r == 0x7f || r < 0x20) {
			t.Errorf("a control character reached the output: %q", got)
			break
		}
	}
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d ends in whitespace: %q", i, line)
		}
	}
}
