package model

import (
	"reflect"
	"testing"
)

// TestSessionEventWire_HasEveryDomainField is the tripwire the round-trip test
// cannot be.
//
// SessionEvent has no struct tags: it marshals through the hand-maintained
// sessionEventWire DTO with field-by-field copies. Add a field to SessionEvent and
// forget the DTO and the field never crosses the wire — which is exactly what
// happened to TunnelReason. Every existing test stayed green, because
// TestSessionEvent_JSONRoundTrip compares Marshal→Unmarshal→Marshal for byte
// identity and a field absent from the DTO on BOTH sides round-trips identically.
// Symmetric loss is invisible to a symmetric test.
//
// This asserts structurally instead: for every exported field on SessionEvent there
// must be one of the same name on sessionEventWire. It cannot be satisfied by
// accident, and it fails at the moment the two drift rather than in the field.
func TestSessionEventWire_HasEveryDomainField(t *testing.T) {
	domain := reflect.TypeOf(SessionEvent{})
	wire := reflect.TypeOf(sessionEventWire{})

	// Deliberate renames, kept explicit so the check stays strict. Duration and
	// Ttfb are milliseconds on the wire, and the DTO fields are named for the
	// unit rather than silently changing what a same-named field means.
	renamed := map[string]string{"Duration": "DurationMs", "Ttfb": "TtfbMs"}

	for i := 0; i < domain.NumField(); i++ {
		f := domain.Field(i)
		if !f.IsExported() {
			continue
		}
		want := f.Name
		if alias, ok := renamed[want]; ok {
			want = alias
		}
		if _, ok := wire.FieldByName(want); !ok {
			t.Errorf("SessionEvent.%s has no sessionEventWire counterpart (looked for %q): "+
				"it will never reach the wire, and the round-trip test cannot see that",
				f.Name, want)
		}
	}
}

// TestSessionEventWireCoversEveryField is a drift guard, not a behaviour test.
//
// SessionEvent's JSON form is hand-maintained in four places: the struct,
// sessionEventWire, MarshalJSON's field list and UnmarshalJSON's. A field added to
// the struct but missed in the wire mapping compiles, passes every in-process
// test, and silently never reaches agentop — which decodes these out of process.
//
// Comparing field COUNTS rather than names on purpose: the two structs
// deliberately differ in spelling (Duration vs DurationMs), so a name check would
// need an exception list that itself goes stale. A count mismatch is the signal
// that someone added to one and not the other.
//
// The count guard is necessary but not sufficient — it cannot see a field that IS
// in sessionEventWire but was forgotten in MarshalJSON or UnmarshalJSON. That half
// is covered by the per-field round-trip tests, of which
// TestSessionEvent_ClientRoundTrips in core/pipeline/session_test.go is one.
func TestSessionEventWireCoversEveryField(t *testing.T) {
	got := reflect.TypeOf(SessionEvent{}).NumField()
	want := reflect.TypeOf(sessionEventWire{}).NumField()
	if got != want {
		t.Fatalf("SessionEvent has %d fields, sessionEventWire has %d.\n"+
			"Add the new field to sessionEventWire AND to both MarshalJSON and "+
			"UnmarshalJSON, or agentop will never see it.", got, want)
	}
}
