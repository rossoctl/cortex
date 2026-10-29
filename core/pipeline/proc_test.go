package pipeline

import (
	"strings"
	"testing"
)

// The process and the naming header ride every event to agentop and into the archive, which
// is where session.Link reads them back from.
func TestSessionEvent_ProcAndSessionHeaderRoundTrip(t *testing.T) {
	in := SessionEvent{Phase: SessionRequest, Proc: "100.5", SessionHeader: "X-Claude-Code-Session-Id"}
	raw, err := in.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"proc":"100.5"`, `"sessionHeader":"X-Claude-Code-Session-Id"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire form %s lacks %s", raw, key)
		}
	}
	var out SessionEvent
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	if out.Proc != in.Proc || out.SessionHeader != in.SessionHeader {
		t.Errorf("after the round trip: Proc %q SessionHeader %q", out.Proc, out.SessionHeader)
	}
	if bare, _ := (SessionEvent{Phase: SessionRequest}).MarshalJSON(); strings.Contains(string(bare), "proc") || strings.Contains(string(bare), "sessionHeader") {
		t.Errorf("an event naming no process carries the keys anyway: %s", bare)
	}
}
