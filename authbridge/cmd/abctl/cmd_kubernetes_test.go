package main

import "testing"

// chooseEndpoint is the whole --kubernetes decision, and the reason it is a
// function: runObserve opens a terminal, so this could not be tested in place.
//
// The row that matters is a running local Cortex under the default --kubernetes:
// before the flag existed the local one won there unconditionally, which left
// someone who runs Cortex on a laptop AND works against a cluster no way to reach
// the picker.
func TestChooseEndpoint(t *testing.T) {
	const local = "http://localhost:47601"
	const explicit = "http://example.test:9094"

	for _, tc := range []struct {
		name       string
		explicit   string
		local      string
		localUp    bool
		kubernetes bool
		want       string
	}{
		{"explicit wins over a live local", explicit, local, true, false, explicit},
		{"explicit wins under --kubernetes", explicit, local, true, true, explicit},
		{"live local is taken when --kubernetes is off", "", local, true, false, local},
		{"live local is skipped under --kubernetes", "", local, true, true, ""},
		{"dead local falls to the picker", "", local, false, false, ""},
		{"dead local falls to the picker under --kubernetes", "", local, false, true, ""},
		{"no local at all", "", "", false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseEndpoint(tc.explicit, tc.local, tc.localUp, tc.kubernetes)
			if got != tc.want {
				t.Errorf("chooseEndpoint(%q, %q, %v, %v) = %q, want %q",
					tc.explicit, tc.local, tc.localUp, tc.kubernetes, got, tc.want)
			}
		})
	}
}

// An empty return is what wires up the picker (main sets opts.Lister only then), so
// "offers the picker" and "chose no endpoint" are the same statement. Asserted
// separately because that coupling is the flag's entire purpose and is easy to
// break by making the empty case return a default address instead.
func TestChooseEndpoint_EmptyMeansPicker(t *testing.T) {
	if got := chooseEndpoint("", "http://localhost:47601", true, true); got != "" {
		t.Errorf("a live local Cortex under --kubernetes must still yield the picker, got %q", got)
	}
}
