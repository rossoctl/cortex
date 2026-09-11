package toolprune

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// anthropicBody is deliberately awkward: unsorted keys, odd indentation, a
// trailing field after tools. Byte-exactness assertions below depend on it
// staying awkward, because the whole safety claim is "every byte outside the
// deleted elements is unchanged".
const anthropicBody = `{"model":"claude-opus-5",
  "tools":[
    {"name":"Read","description":"read a file","input_schema":{"type":"object"}},
    {"name":"NotebookEdit","description":"edit a notebook","input_schema":{"type":"object"}},
    {"name":"Bash","description":"run a command","input_schema":{"type":"object"}}
  ],
  "max_tokens":1024,"stream":true}`

func configured(t *testing.T, remove ...string) *ToolPrune {
	t.Helper()
	p := New()
	raw, err := json.Marshal(map[string]any{"remove": remove})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func inferenceCtx(path, body string, toolNames ...string) *pipeline.Context {
	pctx := &pipeline.Context{Path: path, Body: []byte(body)}
	tools := make([]pipeline.InferenceTool, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, pipeline.InferenceTool{Name: n})
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Tools: tools}
	return pctx
}

func run(t *testing.T, p *ToolPrune, pctx *pipeline.Context, policies ...pipeline.ErrorPolicy) {
	t.Helper()
	var opts []pipeline.Option
	if len(policies) > 0 {
		opts = append(opts, pipeline.WithPolicies(policies...))
	}
	pipe, err := pipeline.New([]pipeline.Plugin{p}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue — tool-prune must never block a request", act.Type)
	}
}

// TestPrune_LeavesEveryOtherByteIntact is the core safety claim. Deleting a
// tool must not reformat the document, reorder keys, or disturb whitespace: the
// request that reaches the model has to be the one the client sent, minus
// exactly the elements named.
func TestPrune_LeavesEveryOtherByteIntact(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx)

	if !pctx.BodyMutated() {
		t.Fatal("expected the body to be rewritten")
	}
	got := string(pctx.Body)
	if strings.Contains(got, "NotebookEdit") {
		t.Errorf("removed tool still present:\n%s", got)
	}
	for _, keep := range []string{
		`"model":"claude-opus-5"`,
		`"name":"Read"`,
		`"name":"Bash"`,
		`"max_tokens":1024`,
		`"stream":true`,
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("expected %s to survive verbatim:\n%s", keep, got)
		}
	}
	// The only difference from the original must be the removed element.
	if len(got) >= len(anthropicBody) {
		t.Errorf("body did not shrink: %d -> %d", len(anthropicBody), len(got))
	}
}

// TestPrune_DescendingDeletion: removing several tools by index only works if
// the deletions run high-to-low. An ascending loop would shift the array under
// itself and delete the wrong elements — here it would leave "Bash" and remove
// something else, so the assertion catches exactly that bug.
func TestPrune_DescendingDeletion(t *testing.T) {
	body := `{"tools":[{"name":"A"},{"name":"B"},{"name":"C"},{"name":"D"},{"name":"E"}]}`
	p := configured(t, "A", "B", "D")
	pctx := inferenceCtx("/v1/messages", body, "A", "B", "C", "D", "E")
	run(t, p, pctx)

	got := string(pctx.Body)
	for _, gone := range []string{`"A"`, `"B"`, `"D"`} {
		if strings.Contains(got, gone) {
			t.Errorf("tool %s should be gone: %s", gone, got)
		}
	}
	for _, kept := range []string{`"C"`, `"E"`} {
		if !strings.Contains(got, kept) {
			t.Errorf("tool %s should remain: %s", kept, got)
		}
	}
}

// TestPrune_OpenAIDialect: OpenAI nests the name under function, Anthropic puts
// it at the top level. Both must resolve, since the plugin reads names out of
// the raw bytes rather than trusting manifest ordering.
func TestPrune_OpenAIDialect(t *testing.T) {
	body := `{"tools":[{"type":"function","function":{"name":"Read"}},` +
		`{"type":"function","function":{"name":"NotebookEdit"}}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/chat/completions", body, "Read", "NotebookEdit")
	run(t, p, pctx)

	got := string(pctx.Body)
	if strings.Contains(got, "NotebookEdit") {
		t.Errorf("removed tool still present: %s", got)
	}
	if !strings.Contains(got, "Read") {
		t.Errorf("kept tool missing: %s", got)
	}
}

// TestPrune_RemovingEveryToolDropsTheKeys: an empty tools array is not a safe
// output — OpenAI rejects `tools: []`, and tool_choice without tools. Drop both
// keys instead, so an over-broad remove list still yields a valid request.
func TestPrune_RemovingEveryToolDropsTheKeys(t *testing.T) {
	body := `{"model":"m","tools":[{"name":"A"},{"name":"B"}],"tool_choice":"auto"}`
	p := configured(t, "A", "B")
	pctx := inferenceCtx("/v1/chat/completions", body, "A", "B")
	run(t, p, pctx)

	got := string(pctx.Body)
	if strings.Contains(got, "tools") {
		t.Errorf("tools key should be gone entirely, not left empty: %s", got)
	}
	if strings.Contains(got, "tool_choice") {
		t.Errorf("tool_choice is invalid without tools; should be dropped: %s", got)
	}
	if !strings.Contains(got, `"model":"m"`) {
		t.Errorf("unrelated fields must survive: %s", got)
	}
}

// TestPrune_UnknownNamesIgnored: a name absent from this request's manifest is
// simply not there — not an error. Drift in the configured list costs savings,
// never correctness.
func TestPrune_UnknownNamesIgnored(t *testing.T) {
	body := `{"tools":[{"name":"Read"}]}`
	p := configured(t, "ToolThatDoesNotExist")
	pctx := inferenceCtx("/v1/messages", body, "Read")
	run(t, p, pctx)

	if pctx.BodyMutated() {
		t.Error("no configured tool was present; body must be untouched")
	}
	if string(pctx.Body) != body {
		t.Errorf("body = %s, want unchanged", pctx.Body)
	}
}

// TestPrune_FailsOpen: malformed, truncated and manifest-less bodies all
// forward the original bytes. A cost optimisation must never break a request.
func TestPrune_FailsOpen(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"tools":[{"name":"NotebookEdit"}`},
		{"truncated mid-string", `{"tools":[{"name":"Notebook`},
		{"tools is not an array", `{"tools":"NotebookEdit"}`},
		{"tools absent", `{"model":"m"}`},
		{"empty body", ``},
		{"empty tools array", `{"tools":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx("/v1/messages", tc.body, "NotebookEdit")
			run(t, p, pctx)
			if pctx.BodyMutated() {
				t.Errorf("body was mutated; must fail open on %s", tc.name)
			}
			if string(pctx.Body) != tc.body {
				t.Errorf("body = %q, want original %q", pctx.Body, tc.body)
			}
		})
	}
}

// TestPrune_PathGate: only inference paths are touched, so an unrelated POST
// through the same proxy is never rewritten.
func TestPrune_PathGate(t *testing.T) {
	body := `{"tools":[{"name":"NotebookEdit"}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/some/other/api", body, "NotebookEdit")
	run(t, p, pctx)
	if pctx.BodyMutated() {
		t.Error("non-inference path must not be pruned")
	}
}

// TestPrune_EmptyRemoveListIsNoop: the shipped default is an empty list, so the
// plugin must be inert until an operator fills it in.
func TestPrune_EmptyRemoveListIsNoop(t *testing.T) {
	p := configured(t)
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
	run(t, p, pctx)
	if pctx.BodyMutated() {
		t.Error("empty remove list must not touch the body")
	}
	if p.Metrics() != nil {
		t.Errorf("no requests acted on; Metrics should be nil, got %+v", p.Metrics())
	}
}

// TestPrune_ObserveModeIsProjection: under on_error: observe the plugin computes
// exactly what it would remove and counts it, while the bytes on the wire stay
// untouched and the invocation is marked Shadow. That is what makes measure-only
// mode possible with one registration and no separate code path.
func TestPrune_ObserveModeIsProjection(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx, pipeline.ErrorPolicyObserve)

	if pctx.BodyMutated() {
		t.Error("observe mode must leave the wire untouched")
	}
	if string(pctx.Body) != anthropicBody {
		t.Errorf("body changed under observe:\n%s", pctx.Body)
	}
	if pctx.Extensions.Invocations == nil {
		t.Fatal("expected invocations to be recorded")
	}
	var sawShadowModify bool
	for _, inv := range pctx.Extensions.Invocations.Inbound {
		if inv.Shadow && inv.Reason == "body_rewritten" {
			sawShadowModify = true
		}
	}
	if !sawShadowModify {
		t.Errorf("expected a Shadow=true body_rewritten invocation, got %+v",
			pctx.Extensions.Invocations.Inbound)
	}

	// The projection must still be countable, and must be reported as a
	// projection rather than a realised saving.
	if p.m.requestsProjected != 1 {
		t.Errorf("requestsProjected = %d, want 1", p.m.requestsProjected)
	}
	if p.m.requestsPruned != 0 {
		t.Errorf("requestsPruned = %d, want 0 under observe", p.m.requestsPruned)
	}
	if p.m.bytesRemoved == 0 {
		t.Error("bytesRemoved must accumulate under observe — that is the projection")
	}
	if !hasMetric(p.Metrics(), "requests projected") {
		t.Errorf("readout should say 'requests projected': %+v", p.Metrics())
	}
}

// TestPrune_EnforceCountsPruned is the enforce-mode counterpart.
func TestPrune_EnforceCountsPruned(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx, pipeline.ErrorPolicyEnforce)

	if p.m.requestsPruned != 1 {
		t.Errorf("requestsPruned = %d, want 1", p.m.requestsPruned)
	}
	if p.m.requestsProjected != 0 {
		t.Errorf("requestsProjected = %d, want 0 under enforce", p.m.requestsProjected)
	}
	if p.m.toolsRemoved != 1 {
		t.Errorf("toolsRemoved = %d, want 1", p.m.toolsRemoved)
	}
	if !hasMetric(p.Metrics(), "removed: NotebookEdit") {
		t.Errorf("per-tool attribution missing: %+v", p.Metrics())
	}
}

// finish drives OnFinish with a given per-tier usage split.
func finish(t *testing.T, p *pricedPrune, pctx *pipeline.Context, input, cacheRead, cacheWrite int) {
	t.Helper()
	pctx.Extensions.Inference.InputTokens = input
	pctx.Extensions.Inference.CacheReadTokens = cacheRead
	pctx.Extensions.Inference.CacheWriteTokens = cacheWrite
	// The cost owner settles once the response is known, then OnFinish aggregates the
	// figure it published.
	p.settle(pctx)
	p.OnFinish(context.Background(), pctx)
}

func pruneOnce(t *testing.T, p *pricedPrune) *pipeline.Context {
	t.Helper()
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p.ToolPrune, pctx)
	if !pctx.BodyMutated() {
		t.Fatal("expected a prune")
	}
	return pctx
}

// TestMetrics_NoUsageYetReportsZero: before any response usage is seen there is
// no ratio to convert bytes with, so report zero with the reason rather than a
// number or a NaN.
func TestMetrics_NoUsageYetReportsZero(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"))
	pruneOnce(t, p)
	m := findMetric(t, p.Metrics(), "tokens saved")
	if m.Value != 0 || m.Note != "no response usage seen yet" {
		t.Errorf("got %+v, want 0 with the missing-sample reason", m)
	}
}

// TestMetrics_AttributesSavingToTheRightTier is the core of the design. The tool
// manifest lives in the cached prefix, so on a cache-miss request the saving
// comes out of cache writes and on a hit out of cache reads. Reporting one
// blended token count would hide which — and the tiers are priced up to 12x
// apart, so that distinction is the whole number.
func TestMetrics_AttributesSavingToTheRightTier(t *testing.T) {
	tests := []struct {
		name                         string
		input, cacheRead, cacheWrite int
		wantRow                      string
	}{
		{"cache miss writes the prefix", 8881, 0, 24701, "tokens saved: cache write"},
		{"cache hit reads the prefix", 26, 24701, 8907, "tokens saved: cache read"},
		{"no caching at all", 40000, 0, 0, "tokens saved: input"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withRates(t, configured(t, "NotebookEdit"))
			pctx := pruneOnce(t, p)
			finish(t, p, pctx, tc.input, tc.cacheRead, tc.cacheWrite)

			ms := p.Metrics()
			got := findMetric(t, ms, tc.wantRow)
			if got.Value <= 0 {
				t.Errorf("%s = %v, want positive", tc.wantRow, got.Value)
			}
			if !strings.HasPrefix(got.Note, "estimate, n=") {
				t.Errorf("note = %q, want it labelled an estimate with its sample", got.Note)
			}
			// No other tier may be credited, and there must be no blended total.
			for _, m := range ms {
				if m.Name == "tokens saved" {
					t.Error("a blended 'tokens saved' row invites multiplying by one rate")
				}
				if strings.HasPrefix(m.Name, "tokens saved: ") && m.Name != tc.wantRow {
					t.Errorf("saving also credited to %q", m.Name)
				}
			}
		})
	}
}

// TestPricing_DefaultsPriceKnownModelsWithoutConfig: the built-in table exists
// so a dollar figure appears with no configuration at all — the difference
// between a number an operator sees and one they never get around to enabling.
func TestPricing_BundledRatesPriceKnownModels(t *testing.T) {
	// The bundled table is what an operator gets with no `pricing:` section at
	// all, since pricing.Build(nil) returns it and BuildWithDeps injects the
	// result. Injecting it directly is the unit-test equivalent.
	p := withRates(t, configured(t, "NotebookEdit"), pricing.Bundled()...)
	pruneWithModel(t, p, "claude-opus-5")

	m := findMetric(t, p.Metrics(), "$ saved")
	if m.Value <= 0 {
		t.Errorf("$ saved = %v, want a figure from the bundled rates", m.Value)
	}
	// Provenance must travel with the number, and the note must state the
	// DIRECTION of the error, not merely that rates are built in — "built-in
	// rates" alone reads as a rounding caveat rather than a systematic bias.
	//
	// That direction INVERTED with this migration, which is why the assertion
	// changed rather than the wording being tidied. The three hand-measured family
	// globs this replaced were taken from a discounted gateway, so they understated
	// anyone paying vendor list. The bundled table IS vendor list, generated from
	// LiteLLM's public map, so now it is the discounted-gateway deployment that is
	// overstated. Same figure, opposite bias, and an operator reading the old note
	// would correct in the wrong direction.
	for _, want := range []string{"bundled", "overstated", "pricing.endpoints"} {
		if !strings.Contains(m.Note, want) {
			t.Errorf("note = %q, missing %q", m.Note, want)
		}
	}
}

// TestPricing_UnknownModelStillUnpriced: the defaults cover a known set, not
// everything. A model in neither the table nor the config is counted, not
// charged at some other model's rate.
func TestPricing_UnknownModelStillUnpriced(t *testing.T) {
	// No entries: no rate anywhere, which is exactly this test's subject.
	p := withRates(t, configured(t, "NotebookEdit"))
	pruneWithModel(t, p, "gcp/gemini-3-pro-preview")

	gap := findMetric(t, p.Metrics(), "requests unpriced")
	if gap.Value != 1 || !strings.Contains(gap.Note, "gemini") {
		t.Errorf("unpriced row = %+v, want 1 naming the model", gap)
	}
	for _, m := range p.Metrics() {
		if m.Name == "$ saved" {
			t.Errorf("$ saved = %v for a model with no rate anywhere, want no row", m.Value)
		}
	}
}

// TestMetrics_TierRatesDifferBy12x pins the reason the tiers are separate. The
// same pruned bytes, priced as a cache write versus a cache read at Anthropic's
// published ratios, differ by more than an order of magnitude. A flat rate would
// be wrong by that factor.
func TestMetrics_TierRatesDifferBy12x(t *testing.T) {
	cfg := func(t *testing.T) *pricedPrune {
		// 1.25x input for a write, 0.1x for a read — Anthropic's published ratios.
		return withRates(t, configured(t, "NotebookEdit"),
			anyEndpoint("*", tierRates(1e-05, 1.25e-05, 1e-06)))
	}

	write := cfg(t)
	finish(t, write, pruneOnce(t, write), 0, 0, 24701)
	read := cfg(t)
	finish(t, read, pruneOnce(t, read), 0, 24701, 0)

	w := findMetric(t, write.Metrics(), "$ saved").Value
	r := findMetric(t, read.Metrics(), "$ saved").Value
	if w <= 0 || r <= 0 {
		t.Fatalf("expected both priced: write=%v read=%v", w, r)
	}
	if ratio := w / r; ratio < 12 || ratio > 13 {
		t.Errorf("cache-write / cache-read cost ratio = %.2f, want ~12.5 (1.25x vs 0.1x input)", ratio)
	}
	// And a per-request figure alongside the total.
	if pr := findMetric(t, write.Metrics(), "$ saved / request"); pr.Value <= 0 {
		t.Errorf("$ saved / request = %v, want positive", pr.Value)
	}
}

// TestMetrics_ConcurrentAccess exercises Metrics() against live counter updates.
// describePipeline calls it from the HTTP handler while requests are in flight,
// so it must be safe under -race.
func TestMetrics_ConcurrentAccess(t *testing.T) {
	p := configured(t, "NotebookEdit")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
				pipe, err := pipeline.New([]pipeline.Plugin{p})
				if err != nil {
					t.Error(err)
					return
				}
				pipe.Run(context.Background(), pctx)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = p.Metrics()
			}
		}()
	}
	wg.Wait()
	if p.m.requestsPruned != 8*50 {
		t.Errorf("requestsPruned = %d, want %d", p.m.requestsPruned, 8*50)
	}
}

func TestConfigure_RejectsUnknownFields(t *testing.T) {
	p := New()
	err := p.Configure(json.RawMessage(`{"remove":["A"],"nope":1}`))
	if err == nil {
		t.Fatal("expected an error for an unknown config field")
	}
	if !strings.Contains(err.Error(), "tool-prune config") {
		t.Errorf("error should name the plugin: %v", err)
	}
}

func TestCapabilities_RequestOnlySoStreamingSurvives(t *testing.T) {
	caps := New().Capabilities()
	if !caps.WritesRequestBody {
		t.Error("must declare WritesRequestBody")
	}
	if caps.WritesResponseBody {
		t.Error("must NOT declare WritesResponseBody — it would cost SSE streaming for nothing")
	}
	if len(caps.RequiresAny) != 1 || caps.RequiresAny[0] != "inference-parser" {
		t.Errorf("RequiresAny = %v, want [inference-parser]", caps.RequiresAny)
	}
}

func hasMetric(ms []pipeline.Metric, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func findMetric(t *testing.T, ms []pipeline.Metric, name string) pipeline.Metric {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q not found in %+v", name, ms)
	return pipeline.Metric{}
}

// TestPrune_NeverRemovesForcedToolChoice: a tool_choice that forces a specific
// tool must keep that tool, whichever dialect spells it. Removing it leaves a
// tool_choice naming a tool absent from the manifest, which providers reject —
// turning a cost optimisation into a 400, the one thing this plugin must never
// do. The rest of the remove list still applies.
func TestPrune_NeverRemovesForcedToolChoice(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "anthropic tool_choice.name",
			body: `{"tools":[{"name":"Read"},{"name":"WebSearch"},{"name":"NotebookEdit"}],` +
				`"tool_choice":{"type":"tool","name":"WebSearch"}}`,
		},
		{
			name: "openai tool_choice.function.name",
			body: `{"tools":[{"type":"function","function":{"name":"Read"}},` +
				`{"type":"function","function":{"name":"WebSearch"}},` +
				`{"type":"function","function":{"name":"NotebookEdit"}}],` +
				`"tool_choice":{"type":"function","function":{"name":"WebSearch"}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both WebSearch (forced) and NotebookEdit are configured for removal.
			p := configured(t, "WebSearch", "NotebookEdit")
			pctx := inferenceCtx("/v1/messages", tc.body, "Read", "WebSearch", "NotebookEdit")
			run(t, p, pctx)

			got := string(pctx.Body)
			if !strings.Contains(got, "WebSearch") {
				t.Errorf("forced tool was removed — request is now invalid:\n  %s", got)
			}
			if strings.Contains(got, "NotebookEdit") {
				t.Errorf("non-forced tool should still be pruned:\n  %s", got)
			}
		})
	}
}

// TestPrune_ToolChoiceAutoDoesNotBlockPruning: "auto" / "none" name no tool, so
// they must not be mistaken for a forced choice and suppress all pruning.
func TestPrune_ToolChoiceAutoDoesNotBlockPruning(t *testing.T) {
	for _, choice := range []string{`"auto"`, `"none"`, `{"type":"auto"}`} {
		t.Run(choice, func(t *testing.T) {
			body := `{"tools":[{"name":"Read"},{"name":"NotebookEdit"}],"tool_choice":` + choice + `}`
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx("/v1/messages", body, "Read", "NotebookEdit")
			run(t, p, pctx)
			if strings.Contains(string(pctx.Body), "NotebookEdit") {
				t.Errorf("tool_choice %s should not suppress pruning:\n  %s", choice, pctx.Body)
			}
		})
	}
}

// TestPrune_PathGateIgnoresQueryString: providers accept query parameters on
// these endpoints, and Claude Code really does send /v1/messages?beta=true. A
// suffix match against the raw target misses every such request and the plugin
// silently does nothing — the least debuggable possible failure, because
// everything looks configured correctly.
func TestPrune_PathGateIgnoresQueryString(t *testing.T) {
	body := `{"tools":[{"name":"Read"},{"name":"NotebookEdit"}]}`
	for _, path := range []string{
		"/v1/messages",
		"/v1/messages?beta=true",
		"/v1/messages?beta=true&x=1",
		"/v1/messages/",
		"/v1/chat/completions?stream=false",
		"https://host/v1/messages?beta=true", // absolute-form target via a proxy
	} {
		t.Run(path, func(t *testing.T) {
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx(path, body, "Read", "NotebookEdit")
			run(t, p, pctx)
			if !pctx.BodyMutated() {
				t.Errorf("path %q was not treated as an inference endpoint", path)
			}
		})
	}
}

// TestPrune_NonInferencePathsStillSkip guards the other direction: loosening the
// gate must not make it match everything.
func TestPrune_NonInferencePathsStillSkip(t *testing.T) {
	body := `{"tools":[{"name":"NotebookEdit"}]}`
	for _, path := range []string{"/mcp", "/v1/models", "/healthz", "/v1/messages/batches"} {
		t.Run(path, func(t *testing.T) {
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx(path, body, "NotebookEdit")
			run(t, p, pctx)
			if pctx.BodyMutated() {
				t.Errorf("path %q must not be pruned", path)
			}
		})
	}
}

// TestPrune_TunnelSkipIsDistinguishable: a CONNECT tunnel has no path. Reporting
// that as a path mismatch sent a real investigation hunting for a routing
// problem when the actual cause was that TLS was never decrypted. The reason
// code has to say which.
func TestPrune_TunnelSkipIsDistinguishable(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("", `{"tools":[{"name":"NotebookEdit"}]}`, "NotebookEdit")
	run(t, p, pctx)

	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) == 0 {
		t.Fatal("expected a skip invocation")
	}
	inv := pctx.Extensions.Invocations.Inbound[0]
	if inv.Reason != "no_path_tunnelled" {
		t.Errorf("reason = %q, want no_path_tunnelled so a tunnel is not mistaken for a routing problem", inv.Reason)
	}
}

// TestPrune_PathMismatchRecordsThePath: a skip that does not say what it saw
// cannot be diagnosed from the session timeline.
func TestPrune_PathMismatchRecordsThePath(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/models", `{"tools":[{"name":"NotebookEdit"}]}`, "NotebookEdit")
	run(t, p, pctx)
	inv := pctx.Extensions.Invocations.Inbound[0]
	if inv.Reason != "path_not_inference" || inv.Path != "/v1/models" {
		t.Errorf("inv = %+v, want path_not_inference with the offending path recorded", inv)
	}
}

// configuredJSON builds a plugin from raw config JSON.
// withRates injects a rate table into p, standing in for what
// plugins.BuildWithDeps does in production. Rates reach the plugin by injection
// now, not through its own config, so every pricing test builds a table.
// pricedPrune is the plugin plus the rate table the COST OWNER holds in production.
//
// The plugin has no resolver any more: it publishes what it removed and reads the priced
// figure back off the record. So a test that wants a dollar figure has to do what the
// pipeline does — let the cost owner settle first.
type pricedPrune struct {
	*ToolPrune
	rates pricing.Resolver
}

func withRates(t *testing.T, p *ToolPrune, entries ...pricing.Entry) *pricedPrune {
	t.Helper()
	tab, err := pricing.NewTable(entries)
	if err != nil {
		t.Fatalf("pricing.NewTable: %v", err)
	}
	return &pricedPrune{ToolPrune: p, rates: pricing.NewRegistry(tab)}
}

// settle stands in for inference-parser: price the response and publish the record,
// including the saving attributed to this plugin.
func (p *pricedPrune) settle(pctx *pipeline.Context) {
	s := costing.Settle(pctx, p.rates)
	costing.Store(pctx, s)
	costing.Publish(pctx, costing.NewRecord(s, costing.Avoided(pctx, p.rates)))
}

// tierRates builds prompt-tier rates from per-token values. Zero means "no rate
// for this tier", which is not the same as a rate of zero — see pricing.Cost.
func tierRates(input, cacheWrite, cacheRead float64) pricing.Rates {
	var r pricing.Rates
	for tier, v := range map[pricing.Tier]float64{
		pricing.TierInput:      input,
		pricing.TierCacheWrite: cacheWrite,
		pricing.TierCacheRead:  cacheRead,
	} {
		if v > 0 {
			r.Base[tier], r.Set[tier] = v, true
		}
	}
	return r
}

// anyEndpoint scopes an entry to every endpoint, which is what these tests want:
// the subject is model rates, not endpoint scoping (covered in authlib/pricing).
func anyEndpoint(model string, r pricing.Rates) pricing.Entry {
	return pricing.Entry{Host: "*", Model: model, Rates: r, Prov: pricing.ProvConfigured}
}

func configuredJSON(t *testing.T, raw string) *ToolPrune {
	t.Helper()
	p := New()
	if err := p.Configure(json.RawMessage(raw)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

// pruneWithModel runs one prune and finishes it as the named model, with a
// cache-write split (the cache-miss shape).
func pruneWithModel(t *testing.T, p *pricedPrune, model string) {
	t.Helper()
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p.ToolPrune, pctx)
	pctx.Extensions.Inference.Model = model
	pctx.Extensions.Inference.CacheWriteTokens = 24701
	// The cost owner runs between the request and OnFinish, exactly as the pipeline
	// orders it: the response is what reveals which tier the saving came out of.
	p.settle(pctx)
	p.OnFinish(context.Background(), pctx)
}

// perModelRates reproduces the Claude family's rate ratios exactly — opus 1.0x,
// sonnet ~0.4x, haiku ~0.2x — with synthetic values, so the assertions below are
// about the plugin honouring per-model rates rather than about any real price.
func perModelRates() []pricing.Entry {
	return []pricing.Entry{
		anyEndpoint("claude-opus-5", tierRates(1e-05, 1.25e-05, 1e-06)),
		anyEndpoint("aws/claude-sonnet-5", tierRates(4e-06, 5e-06, 4e-07)),
		anyEndpoint("aws/claude-haiku-4-5", tierRates(2e-06, 2.5e-06, 2e-07)),
	}
}

// TestPricing_PerModelRatesDiffer is why pricing is keyed by model. Across the
// Claude family the input rate spans roughly 5x (opus 1.0x, sonnet ~0.4x, haiku
// ~0.2x). Charging every request at one rate would misstate the saving by that
// factor depending on which model happened to serve it. The rates below are
// synthetic, chosen to reproduce those ratios exactly.
func TestPricing_PerModelRatesDiffer(t *testing.T) {
	usd := map[string]float64{}
	for _, model := range []string{"claude-opus-5", "aws/claude-sonnet-5", "aws/claude-haiku-4-5"} {
		p := withRates(t, configured(t, "NotebookEdit"), perModelRates()...)
		pruneWithModel(t, p, model)
		usd[model] = findMetric(t, p.Metrics(), "$ saved").Value
		if usd[model] <= 0 {
			t.Fatalf("%s: no cost reported", model)
		}
	}
	// Same saved bytes, same tier — cost must track the model's rate ratios.
	if r := usd["claude-opus-5"] / usd["aws/claude-sonnet-5"]; r < 2.4 || r > 2.6 {
		t.Errorf("opus/sonnet cost ratio = %.2f, want ~2.5", r)
	}
	if r := usd["claude-opus-5"] / usd["aws/claude-haiku-4-5"]; r < 4.9 || r > 5.1 {
		t.Errorf("opus/haiku cost ratio = %.2f, want ~5.0", r)
	}
}

// TestPricing_UnknownModelIsCountedNotGuessed: charging an unpriced model at
// another model's rate would be wrong by up to 5x, so it is reported as a gap.
func TestPricing_UnknownModelIsCountedNotGuessed(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"), perModelRates()...)
	pruneWithModel(t, p, "gcp/gemini-3-pro-preview")

	ms := p.Metrics()
	gap := findMetric(t, ms, "requests unpriced")
	if gap.Value != 1 {
		t.Errorf("requests unpriced = %v, want 1", gap.Value)
	}
	if !strings.Contains(gap.Note, "gcp/gemini-3-pro-preview") {
		t.Errorf("note should name the unpriced model, got %q", gap.Note)
	}
	// Tokens are still counted — only the dollars are withheld.
	if findMetric(t, ms, "tokens saved: cache write").Value <= 0 {
		t.Error("token saving should still be reported for an unpriced model")
	}
	for _, m := range ms {
		if m.Name == "$ saved" && m.Value != 0 {
			t.Errorf("$ saved = %v for an unpriced model, want no charge", m.Value)
		}
	}
}

// TestPrune_ByteExactAgainstJSONReconstruction is the real byte-exactness check.
// The earlier test asserted only that some fragments survived and the body got
// shorter, which passes even if the rewrite reflows the whole document — and it
// removed only a middle element, so the two comma cases that actually differ
// (first and last) were never exercised.
//
// Here the expected output is built by deleting the same elements from the
// ORIGINAL bytes by hand, so any reformatting, key reordering or whitespace
// change fails. Also validates the result with encoding/json, which nothing did.
func TestPrune_ByteExactAgainstJSONReconstruction(t *testing.T) {
	const orig = `{"model":"m","tools":[{"name":"A","x":1},{"name":"B","x":2},{"name":"C","x":3}],"max_tokens":8}`
	cases := []struct {
		remove []string
		want   string
	}{
		{[]string{"A"}, `{"model":"m","tools":[{"name":"B","x":2},{"name":"C","x":3}],"max_tokens":8}`},
		{[]string{"C"}, `{"model":"m","tools":[{"name":"A","x":1},{"name":"B","x":2}],"max_tokens":8}`},
		{[]string{"B"}, `{"model":"m","tools":[{"name":"A","x":1},{"name":"C","x":3}],"max_tokens":8}`},
		{[]string{"A", "C"}, `{"model":"m","tools":[{"name":"B","x":2}],"max_tokens":8}`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.remove, "+"), func(t *testing.T) {
			p := configured(t, tc.remove...)
			pctx := inferenceCtx("/v1/messages", orig, "A", "B", "C")
			run(t, p, pctx)
			if got := string(pctx.Body); got != tc.want {
				t.Errorf("byte mismatch\n got: %s\nwant: %s", got, tc.want)
			}
			var any map[string]any
			if err := json.Unmarshal(pctx.Body, &any); err != nil {
				t.Errorf("result is not valid JSON: %v", err)
			}
		})
	}
}

// TestPrune_ToolChoiceStringForms: "required" / "any" force no specific tool, so
// they must not suppress pruning; an object naming nothing recognisable must.
func TestPrune_ToolChoiceStringForms(t *testing.T) {
	body := `{"tools":[{"name":"Read"},{"name":"NotebookEdit"}],"tool_choice":%s}`
	for _, tc := range []struct {
		choice     string
		wantPruned bool
	}{
		{`"required"`, true},
		{`"any"`, true},
		{`{"type":"required"}`, true},
		{`{"tool":{"name":"NotebookEdit"}}`, false}, // Bedrock-style forced tool: kept
		{`{"unknown_shape":true}`, false},           // cannot interpret: decline
	} {
		t.Run(tc.choice, func(t *testing.T) {
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx("/v1/messages", fmt.Sprintf(body, tc.choice), "Read", "NotebookEdit")
			run(t, p, pctx)
			pruned := !strings.Contains(string(pctx.Body), "NotebookEdit")
			if pruned != tc.wantPruned {
				t.Errorf("tool_choice %s: pruned=%v want %v — body: %s", tc.choice, pruned, tc.wantPruned, pctx.Body)
			}
		})
	}
}

// TestPrune_OpenAIDialectAllRemoved: the all-removed path drops tools and
// tool_choice, and must do so for the OpenAI shape too.
func TestPrune_OpenAIDialectAllRemoved(t *testing.T) {
	body := `{"model":"m","tools":[{"type":"function","function":{"name":"A"}},` +
		`{"type":"function","function":{"name":"B"}}],"tool_choice":"auto"}`
	p := configured(t, "A", "B")
	pctx := inferenceCtx("/v1/chat/completions", body, "A", "B")
	run(t, p, pctx)
	got := string(pctx.Body)
	if strings.Contains(got, "tools") || strings.Contains(got, "tool_choice") {
		t.Errorf("both keys should be dropped: %s", got)
	}
	var any map[string]any
	if err := json.Unmarshal(pctx.Body, &any); err != nil {
		t.Errorf("result is not valid JSON: %v", err)
	}
}

// TestPrune_KeepsToolsCitedByHistory: a provider may reject a request whose
// history references a tool the manifest no longer defines. This arises exactly
// when the plugin is enabled mid-conversation — the config hot-reloads, and the
// scan's rolling window can propose a tool used earlier in the same session.
func TestPrune_KeepsToolsCitedByHistory(t *testing.T) {
	body := `{"tools":[{"name":"Read"},{"name":"WebSearch"},{"name":"NotebookEdit"}],
	  "messages":[
	    {"role":"user","content":"go"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"WebSearch","input":{}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`
	p := configured(t, "WebSearch", "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", body, "Read", "WebSearch", "NotebookEdit")
	run(t, p, pctx)

	got := string(pctx.Body)
	if !strings.Contains(got, "WebSearch") {
		t.Errorf("WebSearch is cited by history and must survive:\n%s", got)
	}
	if strings.Contains(got, "NotebookEdit") {
		t.Errorf("NotebookEdit is uncited and should still be pruned:\n%s", got)
	}
}

// TestPrune_PreservesCacheControlBreakpoint: a prompt-cache breakpoint rides on
// one element — Claude Code marks the last tool. Deleting that element deletes
// the breakpoint, and losing it turns every later turn into a full cache write,
// which costs far more than the definitions saved. The marker must move to the
// last surviving tool.
func TestPrune_PreservesCacheControlBreakpoint(t *testing.T) {
	body := `{"tools":[{"name":"Read"},{"name":"Bash"},` +
		`{"name":"NotebookEdit","cache_control":{"type":"ephemeral"}}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", body, "Read", "Bash", "NotebookEdit")
	run(t, p, pctx)

	got := string(pctx.Body)
	if strings.Contains(got, "NotebookEdit") {
		t.Fatalf("the tool should be pruned: %s", got)
	}
	if !strings.Contains(got, "cache_control") {
		t.Errorf("cache breakpoint was destroyed — every later turn becomes a full cache write:\n%s", got)
	}
	// It must land on the LAST surviving tool, where the prefix ends.
	if !strings.Contains(got, `{"name":"Bash","cache_control":{"type":"ephemeral"}}`) {
		t.Errorf("marker not on the last survivor:\n%s", got)
	}
	var any map[string]any
	if err := json.Unmarshal(pctx.Body, &any); err != nil {
		t.Errorf("invalid JSON after the move: %v", err)
	}
}

// TestPrune_DoesNotDuplicateCacheControl: when a surviving tool already carries a
// breakpoint, adding another would exceed the provider's cache_control limit.
func TestPrune_DoesNotDuplicateCacheControl(t *testing.T) {
	body := `{"tools":[{"name":"Read","cache_control":{"type":"ephemeral"}},` +
		`{"name":"NotebookEdit","cache_control":{"type":"ephemeral"}}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", body, "Read", "NotebookEdit")
	run(t, p, pctx)
	if n := strings.Count(string(pctx.Body), "cache_control"); n != 1 {
		t.Errorf("cache_control appears %d times, want 1: %s", n, pctx.Body)
	}
}

// TestNoteDrift_EmptyFirstManifestDoesNotSpendTheOnce: sync.Once marks itself
// done however the closure returns, so an early return on an empty manifest used
// to disable the stale-list warning permanently — and an empty first manifest is
// the norm on dialects whose tool names the plugin cannot read (Gemini
// functionDeclarations, Bedrock toolSpec nesting). A stale remove list then stayed
// silent in exactly the deployments most likely to have one.
func TestNoteDrift_EmptyFirstManifestDoesNotSpendTheOnce(t *testing.T) {
	p := configured(t, "NeverOffered")

	// Nothing observed: must not consume the guard.
	p.noteDrift(nil)
	p.noteDrift([]pipeline.InferenceTool{})
	if p.driftChecked {
		t.Fatal("the check claims to have run on an empty manifest")
	}

	// A real manifest afterwards must still reach the check.
	p.noteDrift([]pipeline.InferenceTool{{Name: "Read"}})
	if !p.driftChecked {
		t.Error("the check never ran on the first non-empty manifest — an empty one had spent the Once")
	}
}

// TestMetrics_DollarRowsDiscloseTheyAreGross: changing the remove list changes the
// cached prefix, so the next request re-writes the whole prefix at ~1.25x input
// while the recurring saving is ~0.1x on a small delta — tens of requests to break
// even after each change. Counters also reset on the reload that applies the
// change, so the re-warm is invisible exactly when it is paid. A figure that does
// not say it is gross reads as net.
func TestMetrics_DollarRowsDiscloseTheyAreGross(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"), pricing.Bundled()...)
	pruneWithModel(t, p, "claude-opus-5")
	for _, name := range []string{"$ saved", "$ saved / request"} {
		m := findMetric(t, p.Metrics(), name)
		if !strings.Contains(m.Note, "gross") || !strings.Contains(m.Note, "re-warm") {
			t.Errorf("%s note = %q, want it to disclose the figure is gross of cache re-warm", name, m.Note)
		}
	}
}

// TestMetrics_RecoveredPanicIsVisible: fail-open means a panicking plugin looks
// healthy while doing nothing. The pricing migration introduced a nil-interface
// panic on the request path, and this blind spot is why it presented as "pruning
// stopped working" rather than as a crash.
func TestMetrics_RecoveredPanicIsVisible(t *testing.T) {
	p := configured(t, "NotebookEdit")
	p.m.recoveredPanic()

	// Reported even though no request was ever seen — a panic on the FIRST request
	// leaves requestsSeen at zero, which is exactly when the row matters most.
	m := findMetric(t, p.Metrics(), "panics recovered")
	if m.Value != 1 {
		t.Errorf("panics recovered = %v, want 1", m.Value)
	}
	if !strings.Contains(m.Note, "SKIPPED") {
		t.Errorf("note = %q, want it to say pruning was skipped", m.Note)
	}

	// And it survives alongside real traffic.
	pruneWithModel(t, withRates(t, p, pricing.Bundled()...), "claude-opus-5")
	if m := findMetric(t, p.Metrics(), "panics recovered"); m.Value != 1 {
		t.Errorf("panics recovered = %v after traffic, want 1", m.Value)
	}
}

// costing reads this plugin's event STRUCTURALLY, by JSON tag, so it can price a saving
// without importing the plugin. That keeps the dependency pointing the right way — a plugin
// that shrinks a body should know nothing about pricing — at the cost of a coupling the
// compiler cannot see.
//
// This is that coupling, asserted: rename bytesRemoved, bodyBytesAfter or projected and the
// saving silently becomes zero, in a figure operators read as money.
func TestEvent_FieldNamesCostingDependsOn(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"), anyEndpoint("*", tierRates(1e-05, 1.25e-05, 1e-06)))
	pctx := pruneOnce(t, p)
	pctx.Extensions.Inference.Model = "claude-opus-5"
	pctx.Extensions.Inference.CacheWriteTokens = 24701

	got := costing.Avoided(pctx, p.rates)
	if len(got) != 1 {
		t.Fatalf("costing found %d savings, want 1 — the event's field names have drifted: %+v", len(got), got)
	}
	s := got[0]
	if s.Component != "tool-prune" {
		t.Errorf("component = %q, want tool-prune", s.Component)
	}
	if s.TokensAvoided <= 0 {
		t.Errorf("tokensAvoided = %d; bytesRemoved or bodyBytesAfter did not decode", s.TokensAvoided)
	}
	if s.USD <= 0 {
		t.Errorf("usd = %v, want a priced saving", s.USD)
	}
	if !s.Estimated {
		t.Error("saving is not marked Estimated; it comes from a byte ratio, not a tokenizer")
	}
	if s.Projected {
		t.Error("saving is marked Projected, but this prune was applied")
	}
}

// Observe mode measures without applying, and the saving must say so: those bytes went
// upstream and were paid for.
func TestEvent_ObserveModeSavingIsProjected(t *testing.T) {
	// observe is a PIPELINE policy, not plugin config: the plugin measures and skips
	// SetBody, so the original bytes go upstream and get billed.
	p := withRates(t, configured(t, "NotebookEdit"), anyEndpoint("*", tierRates(1e-05, 1.25e-05, 1e-06)))
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p.ToolPrune, pctx, pipeline.ErrorPolicyObserve)
	pctx.Extensions.Inference.Model = "claude-opus-5"
	pctx.Extensions.Inference.CacheWriteTokens = 24701

	got := costing.Avoided(pctx, p.rates)
	if len(got) != 1 {
		t.Fatalf("no saving reported in observe mode: %+v", got)
	}
	if !got[0].Projected {
		t.Error("observe-mode saving is not marked Projected; it would read as money not spent")
	}
}

// Observe mode measures without applying: those bytes went upstream and were billed. So the
// dollars belong in a "would save" row, never in the realized "$ saved" total — the readout
// is the one place an operator reads this as money.
func TestMetrics_ObserveModeDollarsAreNotRealized(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"), anyEndpoint("*", tierRates(1e-05, 1.25e-05, 1e-06)))
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p.ToolPrune, pctx, pipeline.ErrorPolicyObserve)
	pctx.Extensions.Inference.Model = "claude-opus-5"
	pctx.Extensions.Inference.CacheWriteTokens = 24701
	p.settle(pctx)
	p.OnFinish(context.Background(), pctx)

	ms := p.Metrics()
	for _, m := range ms {
		if m.Name == "$ saved" {
			t.Errorf("$ saved = %v in observe mode; that money was spent", m.Value)
		}
		if m.Name == "tokens saved: cache write" {
			t.Errorf("projected tokens landed in a realized tier row: %+v", m)
		}
	}
	would := findMetric(t, ms, "$ would save")
	if would.Value <= 0 {
		t.Errorf("$ would save = %v, want the hypothetical figure", would.Value)
	}
	if !strings.Contains(would.Note, "NOT applied") {
		t.Errorf("note does not say the saving was not applied: %q", would.Note)
	}
	if tw := findMetric(t, ms, "tokens would save"); tw.Value <= 0 {
		t.Errorf("tokens would save = %v, want the projected tokens", tw.Value)
	}
}

// A tier this build does not recognize is counted, but not as input: defaulting would
// misattribute by up to 12.5x and read as a real input saving.
func TestMetrics_UnknownTierIsNotInput(t *testing.T) {
	p := withRates(t, configured(t, "NotebookEdit"), anyEndpoint("*", tierRates(1e-05, 1.25e-05, 1e-06)))
	pctx := pruneOnce(t, p)
	pctx.Extensions.Inference.Model = "claude-opus-5"
	pctx.Extensions.Inference.CacheWriteTokens = 24701
	p.settle(pctx)

	// Rewrite the published record with a tier name from a future build.
	ev, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		t.Fatal("no cost record to rewrite")
	}
	if len(ev.Avoided) != 1 {
		t.Fatalf("expected one saving, got %+v", ev.Avoided)
	}
	ev.Avoided[0].Tier = "cache_read_1h"
	costing.Publish(pctx, ev)

	p.OnFinish(context.Background(), pctx)

	ms := p.Metrics()
	for _, m := range ms {
		if m.Name == "tokens saved: input" {
			t.Errorf("an unknown tier was counted as input: %+v", m)
		}
	}
	unknown := findMetric(t, ms, "tokens saved (tier unknown)")
	if unknown.Value <= 0 {
		t.Errorf("tier-unknown row = %v, want the tokens counted", unknown.Value)
	}
}
