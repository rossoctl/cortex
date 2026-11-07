package sessionevent

import (
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Sink is what a listener records through and reads at record time. *session.Store
// satisfies it. One interface for "feed the control plane" (#1497 item 2).
type Sink interface {
	Append(sessionID string, event pipeline.SessionEvent)
	AppendCapture(sessionID string, event pipeline.SessionEvent, c *pipeline.RequestCapture)
	AppendBucket(sessionID string, event pipeline.SessionEvent) *session.Bucket
	AppendPairCapture(sessionID string, first, second pipeline.SessionEvent, c *pipeline.RequestCapture) *session.Bucket
	AppendTrailing(b *session.Bucket, event pipeline.SessionEvent) bool
	Rekey(oldID, newID string)
	TakesCaptures() bool
	ActiveSession() string
	View(sessionID string) *pipeline.SessionView
}

var _ Sink = (*session.Store)(nil)
