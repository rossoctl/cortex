package usage

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestRecord_UpstreamFailureCountsAsError is the consumer half of issue #1045.
//
// The listeners now record a 502 response event when the upstream call fails at
// the transport level. This asserts what that buys an operator here, because the
// payoff was never in the store — it is that the ERRORS metric and the status
// breakdown stop reading as healthy during an upstream outage.
//
// It is also the proof that the fix is additive rather than a double-count. A
// lone request event contributes NOTHING: counting happens entirely on the
// response half, and an unclaimed request half just expires out of a.pending. So
// both Requests and Errors move 0 → 1, not 1 → 2.
func TestRecord_UpstreamFailureCountsAsError(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Before: the request event alone, which is all a transport failure used to
	// leave behind.
	unpaired := New(WithClock(fixedClock(now)))
	unpaired.Record("s1", reqEvent(now, "req-1"))
	if b := unpaired.Snapshot(time.Minute, BucketWidth, "", GroupStatus).Buckets[0]; b.Requests != 0 || b.Errors != 0 {
		t.Fatalf("lone request event: requests = %d, errors = %d, want 0/0 — "+
			"this is the invisibility #1045 describes, and the baseline the fix improves on",
			b.Requests, b.Errors)
	}

	// After: the paired 502 the listeners now record.
	a := New(WithClock(fixedClock(now)))
	a.Record("s1", reqEvent(now, "req-1"))

	fail := respEvent(now, http.StatusBadGateway, 150*time.Millisecond, "", 0)
	fail.RequestID = "req-1"
	fail.Error = pipeline.TransportError(errRefused{})
	a.Record("s1", fail)

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupStatus).Buckets[0]
	if b.Requests != 1 {
		t.Errorf("requests = %d, want 1", b.Requests)
	}
	if b.Errors != 1 {
		t.Errorf("errors = %d, want 1 — a timed-out or refused upstream must raise the error rate", b.Errors)
	}
	// The status breakdown is the third pane the issue lists: without a status
	// the request fell into the (unlabelled) remainder.
	if got := b.Series["502"]; got.Requests != 1 {
		t.Errorf("series[502].requests = %d, want 1 (want a labelled 502, not the unlabelled remainder); series = %+v",
			got.Requests, b.Series)
	}
	// And the fourth: a non-zero duration keeps it in the latency view.
	if b.LatSamples != 1 {
		t.Errorf("latSamples = %d, want 1", b.LatSamples)
	}
}

// errRefused stands in for a refused dial. The classification of real net errors
// is pinned in pipeline.TestTransportError against actual failed requests; here
// all that matters is that an EventError rides along on the event.
type errRefused struct{}

func (errRefused) Error() string { return "dial tcp 127.0.0.1:1: connect: connection refused" }
