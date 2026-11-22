package main

import (
	"log/slog"
	"path/filepath"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// accountantStateFile is the accountant's Tracker snapshot, under ~/.cortex.
const accountantStateFile = "accountant-state.json"

// newAccountingHost builds the process's accountant host. The Tracker snapshot is a laptop
// feature, decided by the same local-install test the session archive and the ledger use: a
// pod restart starts fresh, and no provider cache outlives an hour anyway (design §5.4).
//
// The options come from accounting.OptionsFrom, the same constructor cmd/cortex-envoy uses, so
// accounting.subscription_weights is read once here (the reloader refuses a reload that changes
// them) the same way in both binaries.
func newAccountingHost(c *config.Config, rates *pricing.Registry) *accounting.Host {
	opts := accounting.OptionsFrom(c, rates)
	if localInstall {
		if dir, err := defaultCortexDir(); err == nil {
			opts.SnapshotPath = filepath.Join(dir, accountantStateFile)
		}
	}
	h := accounting.New(opts)
	slog.Info("accounting enabled — context savings are computed by the accountant",
		"snapshot", opts.SnapshotPath)
	return h
}
