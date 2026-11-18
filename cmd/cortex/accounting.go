package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/accounting/push"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// accountantStateFile is the accountant's Tracker snapshot, under ~/.cortex.
const accountantStateFile = "accountant-state.json"

// machineIDFile is the metrics push's service.instance.id, under ~/.cortex.
const machineIDFile = "machine-id"

// newAccountingHost builds the process's accountant host. The Tracker snapshot is a laptop
// feature, decided by the same local-install test the session archive and the ledger use: a
// pod restart starts fresh, and no provider cache outlives an hour anyway (design §5.4).
//
// The options come from accounting.OptionsFrom, the same constructor cmd/cortex-envoy uses, so
// accounting.subscription_weights is read once here (the reloader refuses a reload that changes
// them) the same way in both binaries.
//
// sink receives each computed Result for the llm.savings.* counters; nil when nothing is pushed.
func newAccountingHost(c *config.Config, rates *pricing.Registry, sink *accounting.Sink) *accounting.Host {
	opts := accounting.OptionsFrom(c, rates, sink)
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

// startMetricsPush starts the metrics push from c's metrics_push block, with the METRICS_PUSH_*
// environment applied, and returns the sink the accountant host observes into and the function
// that flushes and stops the push. Both are no-ops when nothing is pushed: no URL, disabled, or a
// block push.Start refuses — accounting fails open, so a bad URL costs the metrics, not the
// process.
//
// The machine id is kept in ~/.cortex/machine-id on a local install (the same local-install test
// the Tracker snapshot uses). In a cluster it is per process: a pod has no home to keep it in,
// and a restarted pod is a new instance to the collector.
func startMetricsPush(c *config.Config, rates pricing.Resolver) (*accounting.Sink, func(context.Context) error) {
	idPath := ""
	if localInstall {
		if dir, err := defaultCortexDir(); err == nil {
			idPath = filepath.Join(dir, machineIDFile)
		}
	}
	mp, shutdown, err := push.Start(c.MetricsPush.Resolve(os.Getenv), version, idPath)
	// The YAML block was already validated by config.Load, so a refusal here comes from the
	// METRICS_PUSH_* environment: runtime input, so a WARN and no push, never a failed start.
	if err != nil {
		slog.Warn("metrics push off — the metrics_push block was refused", "error", err)
		return nil, func(context.Context) error { return nil }
	}
	return accounting.NewSink(mp, rates), shutdown
}
