package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The ring and the ledger must count the same traffic by the same rules
// (#1566). They used to keep private copies of the error and priceable
// predicates, and only the ring's were ever updated: a truncated OpenAI
// stream (200, model, no usage) read "9/9 priced" on today while the 1h
// window named the gap, and a 403 destination_private was an error on one
// surface and not the other.
//
// Each shape is pinned to its expected counts AND compared against the real
// ring: usage.Aggregator folds the same event and its Snapshot totals must
// equal the ledger's row. The priceable comparison is the one that can drift,
// since the ring decides it in costOf and the ledger in PriceableRequest.
func TestLedger_CountsTheRingShapes(t *testing.T) {
	truncated := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.openai.com",
		Inference: &pipeline.InferenceExtension{Model: "gpt-5.1"}, // no usage block
	}
	refusedPrivate := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 403, Host: "internal",
		Inference: &pipeline.InferenceExtension{Model: "m", TotalTokens: 10},
		Error:     &pipeline.EventError{Kind: pipeline.ErrorKindDestinationPrivate},
	}
	failed := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 502, Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{Model: "m"}, // failed, no usage
	}
	healthy := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{Model: "m", TotalTokens: 100},
	}
	// Usage counts with no TotalTokens. The parsers always fill TotalTokens,
	// so this is hand-built: it pins that the two rules read usage alike.
	failedPartialUsage := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 502, Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{Model: "m", InputTokens: 10},
	}

	for _, tc := range []struct {
		name                    string
		e                       *pipeline.SessionEvent
		wantPriceable, wantErrs int64
	}{
		{"truncated OpenAI stream is a coverage gap, not parity", truncated, 1, 0},
		{"egress-guard refusal is policy working, not an error", refusedPrivate, 1, 0},
		{"failed with no usage is an error and not priceable", failed, 0, 1},
		{"healthy inference is priceable and no error", healthy, 1, 0},
		{"failed with usage counts and no total is priceable", failedPartialUsage, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return at }
			w := newTestWriter(t, t.TempDir(), clock)
			w.Record("s", tc.e)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			rows, _, err := w.Query(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("query returned %d rows, want 1", len(rows))
			}
			r := rows[0]
			if r.PriceableRequests != tc.wantPriceable || r.Errors != tc.wantErrs {
				t.Errorf("ledger row: priceable=%d errors=%d, want priceable=%d errors=%d",
					r.PriceableRequests, r.Errors, tc.wantPriceable, tc.wantErrs)
			}

			agg := usage.New(usage.WithClock(clock))
			agg.Record("s", tc.e)
			totals := agg.Snapshot(time.Hour, time.Minute, "", usage.GroupNone).Totals

			if r.PriceableRequests != totals.PriceableRequests {
				t.Errorf("priceable: ledger says %d, ring says %d — the two surfaces disagree on this shape",
					r.PriceableRequests, totals.PriceableRequests)
			}
			if r.Errors != totals.Errors {
				t.Errorf("errors: ledger says %d, ring says %d — the two surfaces disagree on this shape",
					r.Errors, totals.Errors)
			}
		})
	}
}
