package costevent

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// eventWith builds a SessionEvent carrying raw JSON under the cost-event key.
func eventWith(t *testing.T, key, raw string) *pipeline.SessionEvent {
	t.Helper()
	return &pipeline.SessionEvent{
		Plugins: map[string]json.RawMessage{key: json.RawMessage(raw)},
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name     string
		event    *pipeline.SessionEvent
		wantOK   bool
		wantCost float64
		wantSrc  string
	}{
		{
			name:     "gateway header cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.0421,"source":"gateway-header","daily_total_usd":1.5,"daily_max_usd":10}`),
			wantOK:   true,
			wantCost: 0.0421,
			wantSrc:  SourceGatewayHeader,
		},
		{
			name:     "usage fallback cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.01,"source":"usage-fallback"}`),
			wantOK:   true,
			wantCost: 0.01,
			wantSrc:  SourceUsageFallback,
		},
		{
			// A cache hit or error charges nothing and the plugin emits nothing;
			// a zero that does arrive must not read as a priced free request.
			name:   "zero cost is not priced",
			event:  eventWith(t, PluginName, `{"cost_usd":0,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "negative cost rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":-1,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "malformed JSON rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":`),
			wantOK: false,
		},
		{
			name:   "different plugin key ignored",
			event:  eventWith(t, "tool-prune", `{"cost_usd":5}`),
			wantOK: false,
		},
		{
			name:   "no plugins map",
			event:  &pipeline.SessionEvent{},
			wantOK: false,
		},
		{
			name:   "nil event",
			event:  nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Decode(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("Decode ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
		})
	}
}

// TestEventJSONTagsArePinned guards the wire format. abctl decodes these exact
// tags from a separate module that this phase does not rebuild, so a rename here
// would silently blank its COST column.
func TestEventJSONTagsArePinned(t *testing.T) {
	b, err := json.Marshal(Event{
		CostUSD:       0.25,
		Source:        SourceGatewayHeader,
		DailyTotalUSD: 3.5,
		DailyMaxUSD:   10,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,"daily_max_usd":10}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}
}

func TestMicros(t *testing.T) {
	tests := []struct {
		name string
		usd  float64
		want int64
	}{
		{"whole dollar", 1, 1_000_000},
		{"typical request", 0.0421, 42_100},
		{"rounds to nearest", 0.0000005, 1},
		{"sub-micro rounds to zero", 0.0000001, 0},
		{"large", 1234.5678, 1_234_567_800},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Event{CostUSD: tc.usd}).Micros(); got != tc.want {
				t.Errorf("Micros() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The key names the CONCERN, not the producer.
//
// It used to be the producing plugin's name, pinned by a test to
// litellm-budget-track.Name(). That made moving the producer — which cortex #972 does,
// to inference-parser — a breaking wire change for every live consumer AND for every
// event already sitting in a session store. The framework's own body-mutation event
// established the alternative and said why: "a switch of plugin names in a future
// refactor shouldn't break operators' dashboards" (pipeline/context.go).
func TestKey_NamesTheConcernNotTheProducer(t *testing.T) {
	if Key != "cost" {
		t.Errorf("Key = %q, want %q — a producer name here couples every consumer to one plugin", Key, "cost")
	}
	if Key == PluginName {
		t.Error("Key equals the legacy producer name; the point is that they differ")
	}
}

// A record written under the new key decodes. A record written under the legacy key still
// decodes, because session stores are full of them and a consumer reading history must not
// see a gap where the rename happened.
func TestDecode_ReadsBothKeys(t *testing.T) {
	raw, err := json.Marshal(Event{CostUSD: 0.25, Source: SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{Key, PluginName} {
		t.Run(key, func(t *testing.T) {
			e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{key: raw}}
			got, ok := Decode(e)
			if !ok {
				t.Fatalf("no cost decoded under %q", key)
			}
			if got.CostUSD != 0.25 {
				t.Errorf("CostUSD = %v, want 0.25", got.CostUSD)
			}
		})
	}
}

// The new key wins when both are present. A transitional deployment can have an old
// producer and a new one in the same pipeline; taking the legacy value there would report
// the figure from the component being retired.
func TestDecode_NewKeyWinsOverLegacy(t *testing.T) {
	newRaw, err := json.Marshal(Event{CostUSD: 0.25, Source: SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := json.Marshal(Event{CostUSD: 9.99, Source: SourceUsageFallback})
	if err != nil {
		t.Fatal(err)
	}
	e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{
		Key:        newRaw,
		PluginName: oldRaw,
	}}
	got, ok := Decode(e)
	if !ok {
		t.Fatal("nothing decoded")
	}
	if got.CostUSD != 0.25 {
		t.Errorf("CostUSD = %v, want 0.25 from the new key", got.CostUSD)
	}
}

// Presence and PRICEDNESS are different questions, and Decode only ever answered the
// second: it returns false for a record whose cost is an unsettled zero, meaning a caller
// cannot tell "nobody priced this" from "no record at all".
//
// That distinction is load-bearing for #972: the same record is about to carry avoided
// cost, and a request that could not be priced is exactly the one whose savings figure is
// most interesting. Under Decode's rule that record would vanish entirely.
func TestRecord_PresenceIsNotPricedness(t *testing.T) {
	unpriced, err := json.Marshal(Event{CostUSD: 0, Settled: false, Source: SourceUsageFallback})
	if err != nil {
		t.Fatal(err)
	}
	e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{Key: unpriced}}

	if _, ok := Decode(e); ok {
		t.Error("Decode reported a priced figure for an unsettled zero")
	}
	got, ok := Record(e)
	if !ok {
		t.Fatal("Record did not report the record as PRESENT; a consumer cannot see its non-cost fields")
	}
	if got.Source != SourceUsageFallback {
		t.Errorf("Source = %q, want the record's own value", got.Source)
	}
	if got.Priced() {
		t.Error("Priced() true for an unsettled zero")
	}
}

// Priced() is the rule Decode applies, named and reusable, so a consumer that took the
// record via Record can ask the same question without reimplementing the predicate.
func TestPriced_MatchesDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
		want bool
	}{
		{"positive", Event{CostUSD: 0.25}, true},
		{"settled zero is an answer", Event{CostUSD: 0, Settled: true}, true},
		{"unsettled zero is not", Event{CostUSD: 0}, false},
		{"negative is never", Event{CostUSD: -1, Settled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Priced(); got != tc.want {
				t.Errorf("Priced() = %v, want %v", got, tc.want)
			}
			raw, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{Key: raw}}
			if _, ok := Decode(e); ok != tc.want {
				t.Errorf("Decode ok = %v, but Priced() = %v; the two must agree", ok, tc.want)
			}
		})
	}
}

// No record at all stays a clean negative on both paths.
func TestRecord_AbsentIsNotAnError(t *testing.T) {
	for _, e := range []*pipeline.SessionEvent{
		nil,
		{},
		{Plugins: map[string]json.RawMessage{"tool-prune": []byte(`{"bytesRemoved":1}`)}},
	} {
		if _, ok := Record(e); ok {
			t.Errorf("Record found a record in %+v", e)
		}
		if _, ok := Decode(e); ok {
			t.Errorf("Decode found a cost in %+v", e)
		}
	}
}
