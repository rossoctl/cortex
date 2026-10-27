package archive

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// BenchmarkReplaySince_SixHours is the startup replay's read half over a heavy six hours: twelve
// sessions of 120 turns with 64KB prompts, each event reduced with usage.ReplayCopy as
// cmd/cortex does. Its result sizes cmd/cortex's usageReplayBudget, which bounds the replay but no
// longer delays startup.
func BenchmarkReplaySince_SixHours(b *testing.B) {
	clk := newClock()
	root := b.TempDir()
	a, err := Open(root, WithClock(clk.now))
	if err != nil {
		b.Fatal(err)
	}
	st := session.New(0, 0, 0)
	st.AddRecorder(a)
	for s := range 12 {
		for _, e := range synthSession(int64(100+s), 120, 64<<10) {
			e.At = clk.now()
			st.Append(fmt.Sprintf("s%d", s), e)
		}
	}
	st.Close()
	if err := a.Close(); err != nil {
		b.Fatal(err)
	}
	a, err = Open(root, WithClock(clk.now))
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	b.ResetTimer()
	for b.Loop() {
		var kept []pipeline.SessionEvent
		if _, err := a.ReplaySince(context.Background(), clk.now().Add(-6*time.Hour), func(_ string, e *pipeline.SessionEvent) {
			kept = append(kept, usage.ReplayCopy(e))
		}); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(kept)), "events")
	}
}
