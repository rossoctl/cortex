package accounting

import (
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// The weights in the accounting block reach the host: cortex-envoy once built its host without
// them and priced every subscription saving with the defaults.
func TestOptionsFrom_CarriesSubscriptionWeights(t *testing.T) {
	w := config.DefaultSubscriptionWeights()
	w.Output *= 3
	c := &config.Config{Accounting: &config.AccountingConfig{SubscriptionWeights: &w}}
	o := OptionsFrom(c, nil, nil)
	if o.SubscriptionWeights == nil || *o.SubscriptionWeights != w {
		t.Fatalf("SubscriptionWeights = %+v, want %+v", o.SubscriptionWeights, w)
	}
	h := New(o)
	t.Cleanup(func() { _ = h.Close() })
	if h.weights != w {
		t.Errorf("host weights = %+v, want %+v", h.weights, w)
	}
}

func TestOptionsFrom_DefaultsWithoutABlock(t *testing.T) {
	for _, c := range []*config.Config{nil, {}} {
		o := OptionsFrom(c, nil, nil)
		if o.SubscriptionWeights == nil || *o.SubscriptionWeights != config.DefaultSubscriptionWeights() {
			t.Errorf("SubscriptionWeights = %+v, want the defaults", o.SubscriptionWeights)
		}
	}
}
