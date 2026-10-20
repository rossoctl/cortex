package spend

import "log/slog"

// LogRecorder is the minimum CostRecorder: it logs each charge and records nothing else. It does
// not price the charge and does not reach the ledger, the usage aggregator or the session's money
// totals. Those need the recorder in
// docs/superpowers/specs/2026-10-14-contextguru-savings-and-spend-design.md §2.5, which is not
// built; until it is, this line is the record of a plugin's own spend.
//
// It satisfies the interface contract trivially: slog is safe for concurrent use and never
// touches the session store.
type LogRecorder struct{}

func (LogRecorder) RecordCost(c Charge) {
	agent := ""
	if c.Client != nil {
		agent = c.Client.Name
	}
	slog.Info("plugin spend",
		"plugin", c.Plugin, "purpose", c.Purpose, "session", c.Session, "agent", agent,
		"host", c.Host, "model", c.Model,
		"input", c.Usage.Input, "cacheWrite", c.Usage.CacheWrite, "cacheRead", c.Usage.CacheRead,
		"output", c.Usage.Output, "status", c.Status, "ms", c.Duration.Milliseconds())
}

var _ CostRecorder = LogRecorder{}
