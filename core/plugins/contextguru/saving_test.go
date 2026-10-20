package contextguru

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rossoctl/context-guru/apply"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

const savingKey = "context-guru" + pipeline.PluginEventSuffix

// savingOf reads the published event back through its wire shape, the way settle reads it.
func savingOf(t *testing.T, pctx *pipeline.Context) (map[string]any, bool) {
	t.Helper()
	v, ok := pctx.Extensions.Custom[savingKey]
	if !ok {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, true
}

// A compacted request publishes what it removed, in bytes and in engine tokens, per component,
// so settle can price the saving and attribute it to context-guru.
func TestOnRequest_PublishesTheSaving(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	invokeReq(p, pctx)

	m, ok := savingOf(t, pctx)
	if !ok {
		t.Fatal("no context-guru/event on a compacted request")
	}
	if got, want := m["bytesRemoved"], float64(len(body)-len(pctx.Body)); got != want {
		t.Errorf("bytesRemoved = %v, want %v", got, want)
	}
	if got, want := m["bodyBytesAfter"], float64(len(pctx.Body)); got != want {
		t.Errorf("bodyBytesAfter = %v, want %v", got, want)
	}
	before, _ := m["tokensBefore"].(float64)
	after, _ := m["tokensAfter"].(float64)
	if before <= after || after <= 0 {
		t.Errorf("tokensBefore/After = %v/%v, want before > after > 0", before, after)
	}
	if m["projected"] != nil {
		t.Errorf("projected = %v on an applied write", m["projected"])
	}
	comps, _ := m["components"].([]any)
	if len(comps) != 1 || comps[0].(map[string]any)["component"] != "collapse" ||
		comps[0].(map[string]any)["saved"].(float64) <= 0 {
		t.Errorf("components = %v, want collapse with a positive saving", m["components"])
	}
}

// Under on_error: observe the write is a shadow, so the saving is projected: the bytes went
// upstream and the money was spent.
func TestOnRequest_ObservedSavingIsProjected(t *testing.T) {
	pl, err := pipeline.New([]pipeline.Plugin{configured(t, collapseEngine)},
		pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	pl.Run(context.Background(), pctx)
	m, ok := savingOf(t, pctx)
	if !ok || m["projected"] != true {
		t.Fatalf("event = %v, want projected", m)
	}
}

// A request the engine left alone saved nothing, so it publishes nothing.
func TestOnRequest_NoChangeNoSaving(t *testing.T) {
	p := configured(t, collapseEngine)
	pctx := inferencePctx("/v1/chat/completions", chatBody("short"))
	invokeReq(p, pctx)
	if _, ok := savingOf(t, pctx); ok {
		t.Fatal("published a saving for a request the engine did not change")
	}
}

// PrefixRewritten is true only when a component that acted changed a message the provider had
// already cached; a skipped or reverted report, or a change in the uncached tail, does not count.
func TestSavingEventPrefixRewritten(t *testing.T) {
	run := func(reps ...cgcomponents.Report) *cgcomponents.RunReport {
		return &cgcomponents.RunReport{TokensBefore: 100, TokensAfter: 60, Components: reps}
	}
	acted := func(idx ...int) cgcomponents.Report {
		return cgcomponents.Report{Component: "c", TokensBefore: 50, TokensAfter: 10, ChangedIdx: idx}
	}
	for _, tc := range []struct {
		name string
		tr   apply.Trace
		want bool
	}{
		{"tail only", apply.Trace{CacheAware: true, MaxCachedIdx: 3, Run: run(acted(4, 5))}, false},
		{"inside the cached prefix", apply.Trace{CacheAware: true, MaxCachedIdx: 3, Run: run(acted(2, 5))}, true},
		{"at the boundary", apply.Trace{CacheAware: true, MaxCachedIdx: 3, Run: run(acted(3))}, true},
		{"reverted change ignored", apply.Trace{CacheAware: true, MaxCachedIdx: 3,
			Run: run(cgcomponents.Report{Reverted: true, ChangedIdx: []int{0}})}, false},
		{"skipped change ignored", apply.Trace{CacheAware: true, MaxCachedIdx: 3,
			Run: run(cgcomponents.Report{Skipped: true, ChangedIdx: []int{0}})}, false},
		{"boundary unknown", apply.Trace{CacheAware: true, MaxCachedIdx: -1, Run: run(acted(0))}, false},
		{"not cache-aware", apply.Trace{CacheAware: false, MaxCachedIdx: 3, Run: run(acted(0))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := savingEventFrom(1000, 600, true, tc.tr, nil)
			if ev.PrefixRewritten != tc.want {
				t.Errorf("PrefixRewritten = %v, want %v", ev.PrefixRewritten, tc.want)
			}
		})
	}
}

// A replayed decision is reported as a replay, so the operator can tell a free replay from a paid act.
func TestSavingEventMarksReplays(t *testing.T) {
	tr := apply.Trace{Run: &cgcomponents.RunReport{TokensBefore: 100, TokensAfter: 60, Components: []cgcomponents.Report{
		{Component: "extract_llm", TokensBefore: 50, TokensAfter: 10, Replays: 2},
		{Component: "mask", Skipped: true},
	}}}
	ev := savingEventFrom(1000, 600, true, tr, nil)
	if len(ev.Components) != 1 || ev.Components[0].Component != "extract_llm" || !ev.Components[0].Replay ||
		ev.Components[0].Saved != 40 {
		t.Fatalf("components = %+v, want one replayed extract_llm saving 40", ev.Components)
	}
	if ev.BytesRemoved != 400 || ev.BodyBytesAfter != 600 || ev.Projected {
		t.Fatalf("bytes = %+v", ev)
	}
}

// The wire contract with cost/settle: the event this plugin really publishes is read and priced
// there as a context-guru saving. Fails if either side renames a field.
func TestPublishedSavingIsPricedBySettle(t *testing.T) {
	p := configured(t, collapseEngine)
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	invokeReq(p, pctx)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", InputTokens: 900, OutputTokens: 10}

	var r pricing.Rates
	for _, tier := range []pricing.Tier{pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	got := settle.Avoided(pctx, pricing.NewRegistry(tab))
	if len(got) != 1 || got[0].Component != "context-guru" || got[0].TokensAvoided <= 0 || got[0].USD <= 0 {
		t.Fatalf("Avoided = %+v, want one priced context-guru saving", got)
	}
	m, _ := savingOf(t, pctx)
	before, okB := m["tokensBefore"].(float64)
	after, okA := m["tokensAfter"].(float64)
	if !okB || !okA {
		t.Fatalf("event %v lacks tokensBefore/tokensAfter, the names settle reads", m)
	}
	if want := int(before - after); got[0].TokensAvoided != want {
		t.Errorf("TokensAvoided = %d, want the engine's %d", got[0].TokensAvoided, want)
	}
}
