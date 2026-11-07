package tui

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A response event holds its request's messages too, and the detail view hides them. So a
// prompt matches the request row and not the response row.
func TestDetailHas_RequestRowNotResponse(t *testing.T) {
	inf := &pipeline.InferenceExtension{Model: "m",
		Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "find the NEEDLE-PROMPT"}},
		Completion: "the reply"}
	req := eventRow{event: &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest, Inference: inf}}
	resp := eventRow{event: &pipeline.SessionEvent{Seq: 2, Phase: pipeline.SessionResponse, Inference: inf}}

	if !detailHas(req, "needle-prompt") {
		t.Error("the request row's detail shows the prompt, so it must match (case-insensitive)")
	}
	if detailHas(resp, "needle-prompt") {
		t.Error("the response row's detail hides the messages, so it must not match")
	}
	if !detailHas(resp, "THE REPLY") {
		t.Error("the response row's detail shows the completion, so it must match")
	}
}

// Text that only the raw capture holds matches, because the detail view shows the capture.
func TestDetailHas_RawCapture(t *testing.T) {
	ev := &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest,
		Inference: &pipeline.InferenceExtension{Model: "m"}}
	raw := &pipeline.RawCapture{Request: `{"system":"RAW-ONLY-NEEDLE"}`}

	if detailHas(eventRow{event: ev}, "raw-only-needle") {
		t.Fatal("without the capture there is nothing to match")
	}
	if !detailHas(eventRow{event: ev, raw: raw}, "raw-only-needle") {
		t.Error("the capture is a node of the detail tree, so its text must match")
	}
}

// detailHas and the detail pane's own search must agree on every row and query: a row the
// scan reports must open on a highlighted match, and a row it skips must open on none.
func TestDetailHas_AgreesWithThePane(t *testing.T) {
	tunnel := &pipeline.SessionEvent{Seq: 9, Host: "tunnel-host.example:443"}
	rows := []eventRow{
		{event: &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m",
				Messages: []pipeline.InferenceMessage{{Role: "user", Content: "alpha beta"}}}}},
		{event: &pipeline.SessionEvent{Seq: 2, Phase: pipeline.SessionResponse,
			Inference: &pipeline.InferenceExtension{Model: "m",
				Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "alpha beta"}},
				Completion: "gamma"}}},
		{event: &pipeline.SessionEvent{Seq: 3, Phase: pipeline.SessionRequest, Host: "h"}, tunnel: tunnel},
	}
	for _, r := range rows {
		for _, q := range []string{"alpha", "gamma", "tunnel-host", "absent-text"} {
			m := detailModel(t, "s1", r.event)
			m.search = map[paneID]string{paneDetail: q}
			m.showDetail(r, true)
			pane := len(m.detailDoc.matches) > 0
			if got := detailHas(r, q); got != pane {
				t.Errorf("seq %d, %q: detailHas = %v, the pane shows a match = %v", r.event.Seq, q, got, pane)
			}
		}
	}
}
