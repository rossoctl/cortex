package accounting

import (
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// OptionsFrom is the Options every binary builds its Host from: the process rate table, the
// config's accounting.subscription_weights, and the metrics sink. cmd/cortex and cmd/cortex-envoy
// both start here, so a setting the accounting block carries cannot reach one binary's host and
// not the other's (review finding: cortex-envoy priced every subscription saving with the default
// weights). A binary adds what only it has on top, such as the laptop's SnapshotPath.
func OptionsFrom(c *config.Config, rates pricing.Resolver, sink *Sink) Options {
	var a *config.AccountingConfig
	if c != nil {
		a = c.Accounting
	}
	w := a.Weights()
	return Options{Resolver: rates, SubscriptionWeights: &w, Sink: sink}
}
