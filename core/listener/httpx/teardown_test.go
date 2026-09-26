package httpx

import (
	"context"
	"testing"
	"time"
)

// TestTeardownContext covers both halves of what this function is for, because each half alone
// is a bug the other one hides.
//
// It was untested when three listeners started depending on it: reverting it to a bare
// context.WithoutCancel left the whole suite green, and so did reverting it to the parent
// context, since every listener test that reaches finalization does so on a context nobody
// cancelled. The failures those reverts cause are money-losing and silent — a charge dropped on
// a hangup, or a goroutine pinned for the life of the process — so the properties are asserted
// here, once, where they are stated.
type teardownKey struct{}

func TestTeardownContext(t *testing.T) {
	t.Run("survives a cancelled parent", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()

		ctx, release := TeardownContext(parent)
		defer release()

		if err := ctx.Err(); err != nil {
			t.Fatalf("Err() = %v on a context whose parent is cancelled: pipeline.RunResponseFrame refuses a done context before calling any plugin, so finalization would silently not happen — which is the entire reason this exists", err)
		}
	})

	t.Run("carries a deadline", func(t *testing.T) {
		ctx, release := TeardownContext(context.Background())
		defer release()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("no deadline: detaching removes the only thing that would ever stop this work, so a plugin that blocks holds a goroutine, its buffers and its pctx forever")
		}
		// A window rather than an equality: the clock starts inside the call. The lower bound is
		// what matters — a deadline much shorter than TeardownTimeout would cut finalization
		// short — and the upper bound catches a unit mix-up.
		if left := time.Until(deadline); left <= TeardownTimeout/2 || left > TeardownTimeout {
			t.Errorf("time until deadline = %v, want (%v, %v]", left, TeardownTimeout/2, TeardownTimeout)
		}
	})

	t.Run("the returned cancel releases it", func(t *testing.T) {
		ctx, release := TeardownContext(context.Background())
		release()

		if err := ctx.Err(); err == nil {
			t.Error("Err() = nil after cancel: the caller's defer is what stops the timer, so a context that ignored it would leave one parked per request until it fired")
		}
	})

	t.Run("keeps the parent's values", func(t *testing.T) {
		// WithoutCancel drops cancellation and nothing else, which is what makes this safe to
		// hand to plugins: a request id, a logger or a trace span put on the context upstream is
		// still there for the finalization pass that reports the charge.
		parent := context.WithValue(context.Background(), teardownKey{}, "kept")
		ctx, release := TeardownContext(parent)
		defer release()

		if got := ctx.Value(teardownKey{}); got != "kept" {
			t.Errorf("Value = %v, want %q", got, "kept")
		}
	})
}
