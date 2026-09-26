package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/usage"
)

// The detail pane is where the EXACT reasoning figure lives.
//
// The events table shows one total per row and the spend drawer shows a proportion,
// deliberately: a table column tight enough to be dropped on a 150-column terminal is
// the wrong home for a fifth figure. So this pane is the only place the number itself
// appears per event, and filterForDetail dropping it would leave it nowhere.
func TestFilterForDetail_ResponseKeepsReasoningTokens(t *testing.T) {
	ext := &pipeline.InferenceExtension{
		Model: "claude-opus-5", OutputTokens: 1593, CompletionTokens: 1593,
		ReasoningTokens: 948, PromptTokens: 56, TotalTokens: 1649,
	}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(filterForDetail(raw, pipeline.SessionResponse))

	if !strings.Contains(got, "reasoningTokens") {
		t.Errorf("response detail dropped reasoningTokens, so the figure appears nowhere:\n%s", got)
	}
	if !strings.Contains(got, "948") {
		t.Errorf("response detail dropped the reasoning count:\n%s", got)
	}
	// It must sit beside the figure it is a subset of, not replace it.
	if !strings.Contains(got, "completionTokens") {
		t.Errorf("response detail lost completionTokens:\n%s", got)
	}
}

// A REQUEST row has no reasoning figure to show — reasoning is reported with the
// response — so the key must not leak onto the request side, where it would read as
// a request-time budget rather than a measurement.
func TestFilterForDetail_RequestDropsReasoningTokens(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-5", ReasoningTokens: 948}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(filterForDetail(raw, pipeline.SessionRequest)); strings.Contains(got, "reasoningTokens") {
		t.Errorf("request detail carries reasoningTokens:\n%s", got)
	}
}

// Unreported reasoning must not render as a zero. The field is omitempty, so an
// absent split leaves the key out entirely rather than claiming the model did none.
func TestFilterForDetail_UnreportedReasoningIsAbsentNotZero(t *testing.T) {
	ext := &pipeline.InferenceExtension{
		Model: "gpt-oss-120b", OutputTokens: 244, CompletionTokens: 244,
	}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(filterForDetail(raw, pipeline.SessionResponse)); strings.Contains(got, "reasoningTokens") {
		t.Errorf("a provider reporting no split still shows reasoningTokens:\n%s", got)
	}
}

// THE OTHER HALF OF THE ONE ABOVE, and the half this pane was missing. A provider that
// measured the split and got zero has told us something — the model did no reasoning on
// this call — and that is not the same statement as a provider which never measured.
// omitempty erases the difference on the wire, so the pane has to read the presence bit
// to put it back; otherwise the surface the PR calls the home of the exact figure is the
// one surface that cannot show a zero.
//
// THREE STATES OVER ONE ABSENT KEY, not two, and the middle one is why this is a table.
// Publishing the zero unconditionally survived every test in this package when the suite
// had only the two ends: the no-bits fixture returns at the type assertion — PresentKinds
// is omitempty too, so an all-zero bitfield leaves no key to read — and so never reaches
// the bit test at all. Only a producer that reports OTHER kinds and not reasoning puts the
// bit test on the path. That mutation is mut-n2b, and this row is what kills it.
func TestFilterForDetail_ReportedZeroReasoningIsShownNotHidden(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bits          uint8
		wantZeroShown bool
	}{
		// No bits at all: a producer predating PresentKinds. The value is the only
		// evidence there is, and it says nothing, so the key stays out.
		{"no presence bits", 0, false},
		// Reports output but NOT reasoning: it measured, and reasoning was not among
		// what it measured. Still absent — and this is the row the bit test needs.
		{"other kinds but not reasoning", usage.KindOutput, false},
		// Reports reasoning, and the figure is zero: a measurement of none.
		{"reasoning reported as zero", usage.KindOutput | usage.KindReasoning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &pipeline.InferenceExtension{
				Model: "claude-opus-5", OutputTokens: 244, CompletionTokens: 244,
				ReasoningTokens: 0,
				PresentKinds:    tc.bits,
			}
			raw, err := json.Marshal(map[string]any{"inference": ext})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Every row has to actually reach the gap: omitempty must really have dropped
			// the key, or a row passes on a figure that was never missing.
			if strings.Contains(string(raw), "reasoningTokens") {
				t.Fatalf("fixture does not reach the defect — omitempty kept the key:\n%s", raw)
			}

			got := string(filterForDetail(raw, pipeline.SessionResponse))
			// The VALUE, not just the key: a restored zero that came back as something
			// else would satisfy a key-presence check while misreporting the measurement.
			// filterForDetail returns compact json.Marshal output; the colorizer spaces it.
			shown := strings.Contains(got, `"reasoningTokens":0`)
			if shown != tc.wantZeroShown {
				t.Errorf("reported-zero shown = %v, want %v — the pane cannot distinguish "+
					"\"the model did no reasoning\" from \"this provider does not say\":\n%s",
					shown, tc.wantZeroShown, got)
			}
			// Whatever the verdict, the key must never carry a non-zero figure it invented.
			if strings.Contains(got, "reasoningTokens") && !shown {
				t.Errorf("reasoningTokens is present but not the reported zero:\n%s", got)
			}
			// The bitfield is plumbing, not a figure for the reader.
			if strings.Contains(got, "presentKinds") {
				t.Errorf("the presence bitfield leaked into the display:\n%s", got)
			}
		})
	}
}

// A REPORTED ZERO MUST NOT LEAK ONTO A REQUEST ROW EITHER. restoreReportedZeroSplits runs
// on both phases by design — the allow-list is the one thing that decides visibility — so
// this is what proves that design holds rather than merely being asserted in its comment.
func TestFilterForDetail_RequestDropsAReportedZeroToo(t *testing.T) {
	ext := &pipeline.InferenceExtension{
		Model: "claude-opus-5", ReasoningTokens: 0,
		PresentKinds: usage.KindOutput | usage.KindReasoning,
	}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(filterForDetail(raw, pipeline.SessionRequest)); strings.Contains(got, "reasoningTokens") {
		t.Errorf("request detail carries a restored reasoning zero:\n%s", got)
	}
}
