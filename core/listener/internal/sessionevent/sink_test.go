package sessionevent

import (
	"testing"

	"github.com/rossoctl/cortex/core/session"
)

// A nil store must give an untyped nil Sink: a typed nil inside the interface would compare
// non-nil and panic on the first method call.
func TestSinkOf(t *testing.T) {
	if got := SinkOf(nil); got != nil {
		t.Fatalf("SinkOf(nil) = %#v, want untyped nil", got)
	}
	store := session.New(0, 0, 0)
	defer store.Close()
	if SinkOf(store) == nil {
		t.Fatal("SinkOf(store) = nil, want the store")
	}
}
