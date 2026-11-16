package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionEvent_SourceRoundTrips(t *testing.T) {
	imported := SessionEvent{Source: ImportedSource("claude-code")}
	b, err := json.Marshal(imported)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"source":"imported:claude-code"`) {
		t.Fatalf("marshal = %s, want a source key", b)
	}
	var back SessionEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Source != "imported:claude-code" || !back.Imported() {
		t.Fatalf("decoded Source = %q, Imported() = %v", back.Source, back.Imported())
	}

	live := SessionEvent{}
	b, err = json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"source"`) {
		t.Fatalf("live event marshal = %s, want no source key", b)
	}
	if live.Imported() {
		t.Fatal("live event Imported() = true")
	}
}

// A source that is not a string decodes as a live event, and the other fields stay.
func TestSessionEvent_NonStringSourceDecodesAsLive(t *testing.T) {
	for _, src := range []string{`1`, `{"a":1}`, `["x"]`, `true`, `null`} {
		var e SessionEvent
		if err := json.Unmarshal([]byte(`{"requestId":"r1","source":`+src+`}`), &e); err != nil {
			t.Errorf("source %s: %v", src, err)
			continue
		}
		if e.Source != "" || e.RequestID != "r1" {
			t.Errorf("source %s: Source %q, RequestID %q, want a live event r1", src, e.Source, e.RequestID)
		}
	}
}
