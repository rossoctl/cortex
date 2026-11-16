package session

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

const importedClaude = "imported:claude-code"

func foldOf(sources ...string) *SummaryFold {
	f := NewSummaryFold()
	for _, s := range sources {
		f.Add("s1", &model.SessionEvent{Phase: model.SessionRequest, Source: s})
	}
	return f
}

func TestFold_AllImportedIsMarked(t *testing.T) {
	if got := foldOf(importedClaude, importedClaude).Summary("s1").Source; got != importedClaude {
		t.Fatalf("Source = %q, want %q", got, importedClaude)
	}
	if got := foldOf("").Summary("s1").Source; got != "" {
		t.Fatalf("live Source = %q, want empty", got)
	}
}

func TestFold_OneLiveEventClearsTheMark(t *testing.T) {
	if got := foldOf(importedClaude, importedClaude, "").Summary("s1").Source; got != "" {
		t.Fatalf("Source = %q, want empty after a live event", got)
	}
	if got := foldOf("", importedClaude).Summary("s1").Source; got != "" {
		t.Fatalf("Source = %q, want empty when a live event came first", got)
	}
}

func TestFold_MarkSurvivesJSON(t *testing.T) {
	for name, f := range map[string]*SummaryFold{
		"imported": foldOf(importedClaude),
		"mixed":    foldOf(importedClaude, ""),
	} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			var g SummaryFold
			if err := json.Unmarshal(b, &g); err != nil {
				t.Fatal(err)
			}
			if got, want := g.Summary("s1").Source, f.Summary("s1").Source; got != want {
				t.Fatalf("Source after JSON = %q, want %q (%s)", got, want, b)
			}
			// A live event after the restore still clears the mark.
			g.Add("s1", &model.SessionEvent{Phase: model.SessionRequest})
			if got := g.Summary("s1").Source; got != "" {
				t.Fatalf("Source after restore + live = %q, want empty", got)
			}
		})
	}
}

func TestFold_CloneKeepsMark(t *testing.T) {
	f := foldOf(importedClaude)
	c := f.Clone()
	if got := c.Summary("s1").Source; got != importedClaude {
		t.Fatalf("clone Source = %q, want %q", got, importedClaude)
	}
	c.Add("s1", &model.SessionEvent{Phase: model.SessionRequest})
	if got := f.Summary("s1").Source; got != importedClaude {
		t.Fatalf("a live Add to the clone reached the original: Source = %q", got)
	}
}

func TestFold_OldLiveSessionJSONStaysLive(t *testing.T) {
	var f SummaryFold
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), &f); err != nil {
		t.Fatal(err)
	}
	f.Add("s1", &model.SessionEvent{Phase: model.SessionRequest, Source: importedClaude})
	if got := f.Summary("s1").Source; got != "" {
		t.Fatalf("Source = %q, want empty: the restored history is live", got)
	}
}
