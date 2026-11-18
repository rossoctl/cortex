package model

import (
	"net/http"
	"strings"
	"testing"
)

func TestRecordsOff(t *testing.T) {
	cases := []struct {
		name string
		vals []string
		want bool
	}{
		{"absent", nil, false},
		{"off", []string{"off"}, true},
		{"any case and padding", []string{"  OFF "}, true},
		{"one of a list", []string{"on, off"}, true},
		{"one of several values", []string{"on", "off"}, true},
		{"on", []string{"on"}, false},
		{"a typo records", []string{"of"}, false},
		{"empty", []string{""}, false},
		{"false is not off", []string{"false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tc.vals {
				h.Add(RecordHeader, v)
			}
			if got := RecordsOff(h); got != tc.want {
				t.Fatalf("RecordsOff(%q) = %v, want %v", tc.vals, got, tc.want)
			}
		})
	}
	if RecordsOff(nil) {
		t.Fatal("RecordsOff(nil) = true")
	}
}

// A producer that sends noRecord with the wrong type loses the flag, never the row: it decodes as
// false, the safe direction for a field that can only withhold.
func TestSessionEvent_NoRecordIsLenient(t *testing.T) {
	for _, v := range []string{`"yes"`, `1`, `null`, `{}`} {
		var e SessionEvent
		if err := e.UnmarshalJSON([]byte(`{"at":"2026-11-18T09:00:00Z","direction":"outbound","phase":"request","host":"h","noRecord":` + v + `}`)); err != nil {
			t.Fatalf("noRecord=%s: %v, want the row decoded", v, err)
		}
		if e.NoRecord || e.Host != "h" {
			t.Fatalf("noRecord=%s: NoRecord=%v Host=%q, want false and the row's other fields", v, e.NoRecord, e.Host)
		}
	}
}

// NoRecord round-trips through the wire form and is absent from a recorded row.
func TestSessionEvent_NoRecordWire(t *testing.T) {
	b, err := SessionEvent{NoRecord: true}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back SessionEvent
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if !back.NoRecord {
		t.Fatalf("NoRecord lost in %s", b)
	}
	plain, _ := SessionEvent{}.MarshalJSON()
	if got := string(plain); strings.Contains(got, "noRecord") {
		t.Fatalf("a recorded row carries the key: %s", got)
	}
}
