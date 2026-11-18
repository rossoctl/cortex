package pipeline

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// ResolveClient pins NoRecord from the headers as the client sent them, and removes the header
// so no plugin sees it and the forwarded request does not carry it.
func TestNoRecord_PinnedAndStripped(t *testing.T) {
	h := http.Header{}
	h.Set(model.RecordHeader, "off")
	h.Set("User-Agent", "claude-cli/2.1.266 (external, cli)")
	c := &Context{Headers: h}
	c.ResolveClient()
	if !c.NoRecord() {
		t.Fatal("NoRecord = false for X-Cortex-Record: off")
	}
	if _, ok := c.Headers[http.CanonicalHeaderKey(model.RecordHeader)]; ok {
		t.Fatal("the header is still in Headers after ResolveClient")
	}
	// A plugin that adds it back later changes nothing: the answer was pinned.
	c.Headers.Set(model.RecordHeader, "on")
	if !c.NoRecord() {
		t.Fatal("a later header change unpinned NoRecord")
	}
}

func TestNoRecord_AbsentRecords(t *testing.T) {
	c := &Context{Headers: http.Header{"User-Agent": {"curl/8"}}}
	c.ResolveClient()
	if c.NoRecord() {
		t.Fatal("NoRecord = true with no header")
	}
	if (&Context{}).NoRecord() {
		t.Fatal("NoRecord = true with nil Headers")
	}
}
