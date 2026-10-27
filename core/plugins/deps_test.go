package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/storage"
	"github.com/rossoctl/cortex/core/storage/filestore"
)

// pricedPlugin records what was injected and when, so the tests can assert the
// ordering contract (injection BEFORE Configure) rather than only the end state.
type pricedPlugin struct {
	name           string
	resolver       pricing.Resolver
	hadAtConfigure bool
	configured     bool
}

func (p *pricedPlugin) Name() string { return p.name }
func (p *pricedPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (p *pricedPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *pricedPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *pricedPlugin) SetPricingResolver(r pricing.Resolver) { p.resolver = r }
func (p *pricedPlugin) Configure(json.RawMessage) error {
	p.configured = true
	p.hadAtConfigure = p.resolver != nil
	return nil
}

func registerPriced(t *testing.T, name string) *pricedPlugin {
	t.Helper()
	p := &pricedPlugin{name: name}
	RegisterPlugin(name, func() pipeline.Plugin { return p })
	t.Cleanup(func() { UnregisterPlugin(name) })
	return p
}

func entriesFor(names ...string) []config.PluginEntry {
	out := make([]config.PluginEntry, 0, len(names))
	for _, n := range names {
		out = append(out, config.PluginEntry{Name: n, Config: json.RawMessage(`{}`)})
	}
	return out
}

func TestBuildWithDeps_InjectsResolverBeforeConfigure(t *testing.T) {
	// Ordering is the contract: a plugin's Configure may build rate-dependent state,
	// so an injection that landed afterwards would be silently too late.
	p := registerPriced(t, "test-priced-order")
	reg := pricing.NewRegistry(nil)

	if _, err := BuildWithDeps(entriesFor("test-priced-order"), Deps{Pricing: reg}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if !p.configured {
		t.Fatal("Configure was never called")
	}
	if !p.hadAtConfigure {
		t.Error("resolver was not injected before Configure ran")
	}
	if p.resolver == nil {
		t.Error("resolver was not injected at all")
	}
}

func TestBuildWithDeps_NoResolverLeavesConsumerUncalled(t *testing.T) {
	// A build that opted out of pricing must not inject a typed-nil Resolver: that
	// would make the plugin's `resolver != nil` check true and its calls panic.
	p := registerPriced(t, "test-priced-none")
	if _, err := BuildWithDeps(entriesFor("test-priced-none"), Deps{}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if p.resolver != nil {
		t.Errorf("resolver = %v, want nil when no Deps.Pricing was supplied", p.resolver)
	}
}

// storedPlugin records the store it was handed and whether it had it at Configure.
type storedPlugin struct {
	pricedPlugin
	store          storage.Store
	hadAtConfigure bool
}

func (p *storedPlugin) SetStore(s storage.Store) { p.store = s }
func (p *storedPlugin) Configure(json.RawMessage) error {
	p.hadAtConfigure = p.store != nil
	return nil
}

func registerStored(t *testing.T, name string) *storedPlugin {
	t.Helper()
	p := &storedPlugin{pricedPlugin: pricedPlugin{name: name}}
	RegisterPlugin(name, func() pipeline.Plugin { return p })
	t.Cleanup(func() { UnregisterPlugin(name) })
	return p
}

func TestBuildWithDeps_InjectsTheStoreBeforeConfigure(t *testing.T) {
	p := registerStored(t, "test-stored-order")
	st, err := filestore.Open(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := BuildWithDeps(entriesFor("test-stored-order"), Deps{Store: st}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if p.store != storage.Store(st) {
		t.Errorf("store = %v, want the Deps store", p.store)
	}
	if !p.hadAtConfigure {
		t.Error("the store was not injected before Configure ran")
	}
}

func TestBuildWithDeps_NoStoreLeavesConsumerUncalled(t *testing.T) {
	p := registerStored(t, "test-stored-none")
	if _, err := BuildWithDeps(entriesFor("test-stored-none"), Deps{}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if p.store != nil {
		t.Errorf("store = %v, want nil when no Deps.Store was supplied", p.store)
	}
}

func TestBuild_IsBuildWithDepsWithNoDeps(t *testing.T) {
	// Build and BuildWithSPIFFE had near-identical 40-line bodies. They now
	// delegate, so this asserts the delegation preserved their behaviour.
	p := registerPriced(t, "test-priced-plainbuild")
	if _, err := Build(entriesFor("test-priced-plainbuild")); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !p.configured {
		t.Error("Build did not configure the plugin")
	}
	if p.resolver != nil {
		t.Error("Build injected a resolver")
	}
}

func TestBuildWithSPIFFE_StillConfiguresAndSkipsNilProvider(t *testing.T) {
	p := registerPriced(t, "test-priced-bws")
	if _, err := BuildWithSPIFFE(entriesFor("test-priced-bws"), nil); err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
	if !p.configured {
		t.Error("BuildWithSPIFFE did not configure the plugin")
	}
}

func TestBuildWithDeps_UnknownPluginStillFailsFast(t *testing.T) {
	_, err := BuildWithDeps(entriesFor("test-priced-nonexistent"), Deps{})
	if err == nil {
		t.Fatal("BuildWithDeps accepted an unknown plugin")
	}
	if !strings.Contains(err.Error(), "unknown plugin") {
		t.Errorf("error %q does not say the plugin is unknown", err)
	}
}

func TestPricingConsumerPlugins_FindsRegisteredConsumers(t *testing.T) {
	// The probe exists so a caller that gates Registry construction on actual need
	// can assert its need-detection covers every consumer — a new consumer slipping
	// past the predicate would otherwise silently receive no resolver and report
	// its traffic as unpriced.
	registerPriced(t, "test-priced-probe")
	found := PricingConsumerPlugins()
	var ok bool
	for _, n := range found {
		if n == "test-priced-probe" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("PricingConsumerPlugins() = %v, missing test-priced-probe", found)
	}
}

// inference-parser IS a pricing consumer, and litellm-budget-track is not — the inverse of
// what this test asserted before cortex #972.
//
// The reason it flipped: the component that knows when token counts are FINAL is the one
// that can price them exactly once, and a budget has no business computing the number it
// enforces against. The old arrangement meant a pipeline without the budget plugin showed
// token counts and no money, with the same field silently changing meaning depending on
// configuration.
func TestPricingConsumerPlugins_TracksTheCostOwner(t *testing.T) {
	consumers := PricingConsumerPlugins()
	var hasParser, hasBudget bool
	for _, n := range consumers {
		switch n {
		case "inference-parser":
			hasParser = true
		case "litellm-budget-track":
			hasBudget = true
		}
	}
	if !hasParser {
		t.Errorf("inference-parser is not reported as a pricing consumer, but it owns costing: %v", consumers)
	}
	if hasBudget {
		t.Errorf("litellm-budget-track is reported as a pricing consumer; it bills a settled figure and computes nothing: %v", consumers)
	}
}

// destinationPlugin declares WritesDestination and does nothing else.
type destinationPlugin struct{ name string }

func (p *destinationPlugin) Name() string { return p.name }
func (p *destinationPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true, Description: "test"}
}
func (p *destinationPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *destinationPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func registerDestination(t *testing.T, name string) []config.PluginEntry {
	t.Helper()
	RegisterPlugin(name, func() pipeline.Plugin { return &destinationPlugin{name: name} })
	t.Cleanup(func() { UnregisterPlugin(name) })
	return []config.PluginEntry{{Name: name}}
}

func TestBuildWithDeps_RefusesADestinationWriterTheListenerCannotHonor(t *testing.T) {
	entries := registerDestination(t, "test-dest-refused")
	_, err := BuildWithDeps(entries, Deps{Listener: pipeline.ListenerSupport{Listener: "reverse proxy"}})
	if err == nil {
		t.Fatal("BuildWithDeps admitted a WritesDestination plugin for a listener that cannot redirect")
	}
	for _, want := range []string{`"test-dest-refused"`, "WritesDestination", "reverse proxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestBuildWithDeps_ZeroDepsRefusesADestinationWriter(t *testing.T) {
	entries := registerDestination(t, "test-dest-zero")
	if _, err := BuildWithDeps(entries, Deps{}); err == nil {
		t.Fatal("a build that names no listener admitted a WritesDestination plugin")
	}
}

func TestBuildWithDeps_AdmitsADestinationWriterWhereHonored(t *testing.T) {
	entries := registerDestination(t, "test-dest-ok")
	deps := Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}}
	if _, err := BuildWithDeps(entries, deps); err != nil {
		t.Fatalf("BuildWithDeps refused a WritesDestination plugin the listener honors: %v", err)
	}
}

// An entry under on_error: off is dropped before it is built, so a plugin the
// listener cannot honor is no obstacle while it is switched off.
func TestBuildWithDeps_DoesNotCheckAnEntryThatIsOff(t *testing.T) {
	entries := registerDestination(t, "test-dest-off")
	entries[0].OnError = pipeline.ErrorPolicyOff
	if _, err := BuildWithDeps(entries, Deps{}); err != nil {
		t.Fatalf("BuildWithDeps checked an entry that is off: %v", err)
	}
}

// historyPlugin records the history it was handed and whether it had it at Configure.
type historyPlugin struct {
	pricedPlugin
	history        session.History
	hadAtConfigure bool
}

func (p *historyPlugin) SetHistory(h session.History) { p.history = h }
func (p *historyPlugin) Configure(json.RawMessage) error {
	p.hadAtConfigure = p.history != nil
	return nil
}

type noHistory struct{}

func (noHistory) Earlier(string, uint64, func(*pipeline.SessionEvent) bool) error { return nil }

func TestBuildWithDeps_InjectsTheHistoryBeforeConfigure(t *testing.T) {
	p := &historyPlugin{pricedPlugin: pricedPlugin{name: "test-history-order"}}
	RegisterPlugin(p.name, func() pipeline.Plugin { return p })
	t.Cleanup(func() { UnregisterPlugin(p.name) })

	if _, err := BuildWithDeps(entriesFor(p.name), Deps{History: noHistory{}}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if p.history != session.History(noHistory{}) {
		t.Errorf("history = %v, want the Deps history", p.history)
	}
	if !p.hadAtConfigure {
		t.Error("the history was not injected before Configure ran")
	}
}

// narrowingPlugin reports ceiling until Configure, and configured after it: the shape
// of a plugin whose config decides how much of its ceiling it uses.
type narrowingPlugin struct {
	name                string
	ceiling, configured pipeline.PluginCapabilities
	done                bool
}

func (p *narrowingPlugin) Name() string { return p.name }
func (p *narrowingPlugin) Capabilities() pipeline.PluginCapabilities {
	if p.done {
		return p.configured
	}
	return p.ceiling
}
func (p *narrowingPlugin) Configure(json.RawMessage) error { p.done = true; return nil }
func (p *narrowingPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *narrowingPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func registerNarrowing(t *testing.T, name string, ceiling, configured pipeline.PluginCapabilities) []config.PluginEntry {
	t.Helper()
	RegisterPlugin(name, func() pipeline.Plugin {
		return &narrowingPlugin{name: name, ceiling: ceiling, configured: configured}
	})
	t.Cleanup(func() { UnregisterPlugin(name) })
	return entriesFor(name)
}

// The listener check holds for the capabilities the pipeline runs, not only for the
// ceiling: a plugin that takes WritesDestination on at Configure is refused wherever a
// plugin declaring it up front would be.
func TestBuildWithDeps_RefusesADestinationWriterThatWidensIntoIt(t *testing.T) {
	widened := pipeline.PluginCapabilities{WritesDestination: true}
	for _, listener := range []pipeline.ListenerSupport{{Listener: "reverse proxy"}, {}} {
		entries := registerNarrowing(t, "test-widen-dest", pipeline.PluginCapabilities{}, widened)
		_, err := BuildWithDeps(entries, Deps{Listener: listener})
		if err == nil {
			t.Fatalf("%s admitted a plugin that widened into WritesDestination", listener.Name())
		}
		for _, want := range []string{`"test-widen-dest"`, "WritesDestination", listener.Name()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
		UnregisterPlugin("test-widen-dest")
	}

	entries := registerNarrowing(t, "test-widen-dest-ok", pipeline.PluginCapabilities{}, widened)
	deps := Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}}
	if _, err := BuildWithDeps(entries, deps); err != nil {
		t.Fatalf("a listener that honors WritesDestination refused a plugin widening into it: %v", err)
	}
}

func TestBuildWithDeps_AdmitsAPluginThatNarrowsAwayWhatTheListenerCannotHonor(t *testing.T) {
	entries := registerNarrowing(t, "test-narrow-dest", pipeline.PluginCapabilities{WritesDestination: true}, pipeline.PluginCapabilities{})
	if _, err := BuildWithDeps(entries, Deps{Listener: pipeline.ListenerSupport{Listener: "reverse proxy"}}); err != nil {
		t.Fatalf("a plugin whose config drops WritesDestination was refused: %v", err)
	}
}

// The pipeline a narrowed plugin is built into sees the narrower set, so a plugin whose
// config turns off its response rewriting no longer buffers every response.
func TestBuildWithDeps_AConfiguredPluginMayNarrowItsCapabilities(t *testing.T) {
	entries := registerNarrowing(t, "test-narrow",
		pipeline.PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true},
		pipeline.PluginCapabilities{WritesRequestBody: true})
	p, err := BuildWithDeps(entries, Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps refused a narrowed plugin: %v", err)
	}
	if p.WritesResponseBody() {
		t.Error("the pipeline still reports WritesResponseBody for a plugin that narrowed it away")
	}
	if !p.WritesRequestBody() {
		t.Error("the pipeline lost the WritesRequestBody the plugin kept")
	}
}

// Widening is reported, not refused: nothing enforced the old "capabilities never vary"
// rule, so refusing it now would break a plugin that relied on that. The pipeline uses
// what the configured instance declares, as it always has.
func TestBuildWithDeps_AdmitsAConfiguredPluginThatWidens(t *testing.T) {
	entries := registerNarrowing(t, "test-widen",
		pipeline.PluginCapabilities{WritesRequestBody: true},
		pipeline.PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true})
	p, err := BuildWithDeps(entries, Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps refused a plugin that widens: %v", err)
	}
	if !p.WritesResponseBody() {
		t.Error("the pipeline dropped the capability the configured instance declared")
	}
}

func TestBuildWithDeps_WarnsOfADifferenceFromTheCeiling(t *testing.T) {
	cases := []struct {
		name                string
		ceiling, configured pipeline.PluginCapabilities
		want                string
	}{
		{"widened", pipeline.PluginCapabilities{WritesRequestBody: true},
			pipeline.PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}, "difference=WritesResponseBody"},
		{"narrowed to a pure reader", pipeline.PluginCapabilities{WritesRequestBody: true},
			pipeline.PluginCapabilities{ReadsBody: true}, `difference="ReadsBody without a body writer"`},
		{"RequiresAny name dropped", pipeline.PluginCapabilities{RequiresAny: []string{"a", "b"}},
			pipeline.PluginCapabilities{RequiresAny: []string{"b"}}, `difference="RequiresAny without \"a\""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			entries := slices.Concat(registerNarrowing(t, "b", pipeline.PluginCapabilities{}, pipeline.PluginCapabilities{}),
				registerNarrowing(t, "test-ceiling-warn", tc.ceiling, tc.configured))
			if _, err := BuildWithDeps(entries, Deps{}); err != nil {
				t.Fatalf("BuildWithDeps: %v", err)
			}
			line := logs.String()
			for _, want := range []string{"level=WARN", "the catalog misdescribes it", "plugin=test-ceiling-warn", tc.want} {
				if !strings.Contains(line, want) {
					t.Errorf("log %q does not contain %s", line, want)
				}
			}
			UnregisterPlugin("test-ceiling-warn")
			UnregisterPlugin("b")
		})
	}
}

func TestBuildWithDeps_DoesNotWarnOfANarrowingWithinTheCeiling(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	entries := registerNarrowing(t, "test-ceiling-quiet",
		pipeline.PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true, RequiresAny: []string{"a"}},
		pipeline.PluginCapabilities{WritesRequestBody: true})
	if _, err := BuildWithDeps(entries, Deps{}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a narrowing within the ceiling logged %q", logs.String())
	}
}

// The catalog keeps reporting the ceiling: it is built from an unconfigured instance, so
// a narrowed build cannot shrink what /v1/plugins advertises.
func TestCatalog_ReportsTheCeilingOfANarrowingPlugin(t *testing.T) {
	registerNarrowing(t, "test-narrow-catalog",
		pipeline.PluginCapabilities{WritesResponseBody: true, Description: "test"},
		pipeline.PluginCapabilities{})
	resetCatalogCache()
	t.Cleanup(resetCatalogCache)
	for _, e := range Catalog() {
		if e.Name == "test-narrow-catalog" {
			if !e.Capabilities.WritesResponseBody {
				t.Error("the catalog lost the ceiling's WritesResponseBody")
			}
			return
		}
	}
	t.Fatal("test-narrow-catalog missing from the catalog")
}
