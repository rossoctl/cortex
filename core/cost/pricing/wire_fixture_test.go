package pricing

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/internal/wirefixture"
)

// agentop decodes both fixtures too (cmd_pricing.go), so a renamed key on either side
// fails a test. Some row sets each optional key: gw.example has a unit, a multiplier and
// a threshold. zenHost has the shipped free models. A model priced only for
// other.example is unpriced at both. Free and a unit cannot meet in one host's answer,
// because a non-USD endpoint does not resolve the shipped USD rows. Thus the
// effective fixture is checked against two hosts.
func TestHandler_CoversTheWireFixtures(t *testing.T) {
	half := 0.5
	rates := TierRates{InputCostPerMillion: 1, CacheWriteCostPerMillion: 1, CacheReadCostPerMillion: 1, OutputCostPerMillion: 1}
	tab := mustBuild(t, &Config{Endpoints: []EndpointConfig{
		{Hosts: []string{"gw.example"}, Unit: "credits", Multiplier: &half, Models: map[string]ModelConfig{
			"gw-model": {TierRates: rates, Above: []ThresholdConfig{{PromptTokens: 1000, TierRates: rates}}},
		}},
		{Hosts: []string{"other.example"}, Models: map[string]ModelConfig{"only-elsewhere": {TierRates: rates}}},
	}})
	tab.listFetchedAt = time.Now()
	h := NewRegistry(tab).Handler()
	get := func(target string) []byte {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w.Body.Bytes()
	}

	wirefixture.Covers(t, "testdata/pricing-table.json", get("/pricing/table"))
	wirefixture.Covers(t, "testdata/pricing-effective.json",
		get("/pricing/table?host=gw.example"), get("/pricing/table?host="+zenHost))
}
