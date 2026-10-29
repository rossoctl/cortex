package session

import (
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

type evictRecorder struct{ evicted []string }

func (r *evictRecorder) Record(string, *pipeline.SessionEvent) {}
func (r *evictRecorder) Evicted(id string)                     { r.evicted = append(r.evicted, id) }

// Both ways the store drops a session on its own tell every Evicter: the attributor keeps its
// own list of live sessions, and one it is not told about stays live there, so a tool bound to
// it keeps filing calls under an id the store no longer holds.
func TestStore_TellsEvictersWhatItDrops(t *testing.T) {
	t.Run("max_sessions", func(t *testing.T) {
		s := New(0, 0, 1)
		defer s.Close()
		r := &evictRecorder{}
		s.AddRecorder(r)
		s.Append("s1", ev())
		s.Append("s2", ev())
		if !slices.Equal(r.evicted, []string{"s1"}) {
			t.Errorf("evicted = %v, want [s1]", r.evicted)
		}
	})
	t.Run("ttl", func(t *testing.T) {
		clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
		s := New(time.Minute, 0, 0, WithClock(clk.now))
		defer s.Close()
		r := &evictRecorder{}
		s.AddRecorder(r)
		s.Append("s1", ev())
		clk.advance(2 * time.Minute)
		s.Cleanup()
		if !slices.Equal(r.evicted, []string{"s1"}) {
			t.Errorf("evicted = %v, want [s1]", r.evicted)
		}
	})
}
