package ledger

import "github.com/rossoctl/cortex/core/session"

// Writer is a session.Recorder beside the usage aggregator; this fails the build if either
// signature drifts.
var _ session.Recorder = (*Writer)(nil)
