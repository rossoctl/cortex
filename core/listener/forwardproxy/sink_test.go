package forwardproxy

import "testing"

// A Server with no store must give an untyped nil Sink: a typed nil inside the interface
// would compare non-nil and panic on the first method call.
func TestSinkNilWithoutStore(t *testing.T) {
	if got := (&Server{}).sink(); got != nil {
		t.Fatalf("sink() = %#v, want untyped nil", got)
	}
}
