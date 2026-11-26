package ledger

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// detachedEvent is the event core/accounting.Host writes for a detached model call (cortex#2140).
func detachedEvent(t *testing.T, sp event.SpendRecord) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(event.Event{Purpose: event.PurposeDetachedSpend, Thread: "main", Spends: []event.SpendRecord{sp}})
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.anthropic.com", RequestID: "detached:r1:1",
		Inference: &pipeline.InferenceExtension{Model: "claude-haiku-4-5"},
		Plugins:   map[string]json.RawMessage{event.Key: rec},
	}
}

// A detached model call's record books its spend as plugin spend, and counts as no request.
func TestRow_ADetachedSpendIsPluginSpendNotARequest(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	w.Record("s1", detachedEvent(t, event.SpendRecord{Operator: "context-guru", Basis: event.BasisUSD, USD: 0.0195}))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.PluginSpendMicros != 19_500 {
		t.Errorf("PluginSpendMicros = %d, want 19500", r.PluginSpendMicros)
	}
	if r.Requests != 0 || r.CostMicros != 0 || r.Tokens != 0 || r.PricedRequests != 0 || r.PriceableRequests != 0 ||
		r.Errors != 0 || r.KeepAlivePings != 0 {
		t.Errorf("a detached spend reached the request figures: %+v", r.Counts)
	}
}

// A detached spend that no total counts (refused, here) writes no row.
func TestRow_ARefusedDetachedSpendWritesNoRow(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	w.Record("s1", detachedEvent(t, event.SpendRecord{Basis: event.BasisUSD, Refused: true}))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("%d rows, want 0: %+v", len(rows), rows)
	}
}
