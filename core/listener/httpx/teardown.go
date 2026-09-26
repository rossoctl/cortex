package httpx

import (
	"context"
	"time"
)

// TeardownTimeout bounds work that outlives the request it belongs to.
//
// Ten seconds because the work is finalization — settle a figure, append a session event,
// dispatch finishers — not I/O to an upstream. Long enough that a slow ledger append or a
// plugin's own timeout completes, short enough that a wedged finisher cannot pin a goroutine
// and the buffers it holds for the life of the process.
//
// IT IS ALSO HOW LONG A CALLER CAN BLOCK, which is a real change in behaviour and not only a bound on
// background work. Every finalization site dispatches SYNCHRONOUSLY — the reverse proxy's inside
// Read/Close on the response body, ext_proc's inside the Process defer — so a wedged plugin now holds
// that call for up to this long, where before it returned at once. It returned at once by NOT DOING
// THE WORK: the dispatch was refused on a cancelled context, which is the defect this exists to fix.
// So the choice is between blocking for a bounded time and losing the charge, and ten seconds is the
// price put on the first.
//
// NOT CONFIGURABLE, deliberately: an operator has no information with which to tune it, and a knob
// here would be a knob on how long a hangup can hold a connection slot. If a plugin needs longer than
// this to finalize, the plugin is what needs the deadline.
const TeardownTimeout = 10 * time.Second

// TeardownContext detaches ctx from its parent's cancellation and gives it a deadline.
//
// DETACHED, BECAUSE THE PARENT IS ALREADY DONE. Finalization runs exactly when a request has
// ended — a client hangup, a stream torn down, a response fully written — so the request's own
// context is cancelled and pipeline.RunResponseFrame refuses a cancelled context before calling
// any plugin. Passing it straight through means the work silently does not happen, which is the
// defect this exists to prevent.
//
// AND BOUNDED, WHICH context.WithoutCancel ALONE IS NOT. Detaching removes the only thing that
// would ever stop this work: with no deadline, a plugin that blocks holds a goroutine, its
// buffers and its pctx forever, and nothing in the process notices. The caller must call the
// returned cancel — deferring it is what releases the timer.
func TeardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), TeardownTimeout)
}
