// Package spend lets a plugin report money it spent OUTSIDE the request pipeline.
//
// Cortex prices and records every request it proxies: inference-parser settles the response
// and the session store's recorders write the ledger. A call a plugin makes on its own client
// never passes through that path — a keep-alive ping fired from a background sweep, or a
// compaction model call — so without this it is money the caller's credential paid that no
// Cortex figure shows. See docs/superpowers/specs/2026-10-14-contextguru-savings-and-spend-design.md.
//
// The plugin reports TOKENS, never dollars. The recorder prices them with the process's own
// rate table, so a plugin's spend gets the same provenance, currency and plausibility rules
// as a real request's, and there is one pricing site rather than one per plugin.
package spend

import (
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Purposes a Charge is made for. A wire string: hyphenated lowercase, like event.Source.
const (
	// PurposeKeepAlive is a ping that kept an idle session's prompt cache warm.
	PurposeKeepAlive = "keep-alive"
	// PurposeCompaction is a model call a plugin made to shrink a request (an extraction or a
	// summary).
	PurposeCompaction = "compaction"
)

// Charge is one upstream call a plugin made and paid for on a session's behalf.
type Charge struct {
	// Plugin is the plugin that made the call, e.g. "context-guru".
	Plugin string
	// Purpose is PurposeKeepAlive or PurposeCompaction. Separate purposes answer separate
	// questions ("is keep-alive worth it?", "is compaction worth it?"), so they are not merged.
	Purpose string
	// Session is the session the call served. A charge with no session is dropped.
	Session string
	// Client is the agent the session belongs to, from the request that caused the call. The
	// ledger's agent label is built from it.
	Client *pipeline.EventClient
	// Host is the endpoint billed; Model the model the call ran. Both label the ledger row and
	// select the rates.
	Host  string
	Model string
	// Usage is the call's tokens by tier, as the provider reported them.
	Usage pricing.Usage
	// Status is the upstream HTTP status.
	Status int
	// At is when the call finished; Duration how long it took. Zero Duration means not measured.
	At       time.Time
	Duration time.Duration
}

// CostRecorder records a Charge in Cortex's cost record: the ledger, the usage aggregator and
// the session's money totals.
//
// Contract for implementations:
//   - Safe for concurrent use. Callers include background sweeps and model-call paths.
//   - Does not block on I/O: some callers hold a request open while they call it.
//   - Never makes the session active or refreshes it. A ping is not agent activity, and
//     counting it as such would distort the idle tracking that keep-alive depends on.
//   - May drop a charge for a session that no longer exists, rather than re-create the
//     session to hold it.
type CostRecorder interface {
	RecordCost(Charge)
}

// RecorderConsumer is implemented by plugins that spend money outside the pipeline.
// plugins.BuildWithDeps injects the process's CostRecorder before Configure runs, the same
// contract as pricing.ResolverConsumer. A plugin must tolerate never being called: a binary
// that wires no recorder injects nothing, and the plugin then only logs its spend.
type RecorderConsumer interface {
	SetCostRecorder(CostRecorder)
}
