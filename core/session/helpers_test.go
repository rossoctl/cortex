package session

import (
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The aggregator implements these through optional interfaces, so a signature drift on
// either side would degrade to a silent skip. This makes it a compile error instead. It
// lives here because usage cannot import session.
var _ Rekeyer = (*usage.Aggregator)(nil)
var _ Clearer = (*usage.Aggregator)(nil)
var _ Deleter = (*usage.Aggregator)(nil)

func ev() pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest}
}

// procTestClock is a store clock tests move by hand.
type procTestClock struct{ t time.Time }

func (c *procTestClock) now() time.Time          { return c.t }
func (c *procTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newProcStore() (*Store, *procTestClock) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	return New(0, 0, 0, WithClock(clk.now)), clk
}

// recordIn appends one request to id a second after the last.
func recordIn(s *Store, clk *procTestClock, id string) {
	clk.advance(time.Second)
	s.Append(id, pipeline.SessionEvent{At: clk.now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
}

type rekeyRecorder struct{ rekeyed [][2]string }

func (r *rekeyRecorder) Record(string, *pipeline.SessionEvent) {}
func (r *rekeyRecorder) Rekeyed(o, n string)                   { r.rekeyed = append(r.rekeyed, [2]string{o, n}) }

// claimedBy is a request whose header named its session, from agent's client.
func claimedBy(agent string) pipeline.SessionEvent {
	e := ev()
	e.SessionHeader = ClaudeCodeSessionHeader
	e.Client = &pipeline.EventClient{Name: agent}
	return e
}
