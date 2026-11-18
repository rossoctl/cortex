package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// chooseEndpoint is the whole --kubernetes decision, and the reason it is a
// function: runObserve opens a terminal, so this could not be tested in place.
//
// The row that matters is a running local Cortex WITH --kubernetes passed: that is
// the case the flag exists for, since the probe would otherwise win every time and
// leave someone who runs Cortex on a laptop AND works against a cluster no way to
// reach the picker. Without the flag the local one wins, which is the default and
// the quickstart.
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
		{"live local is taken by default", "", local, true, false, local},
		// An empty return is what wires up the picker (main sets opts.Lister only then).
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

// The flag's DEFAULT is the point of this file, and chooseEndpoint cannot pin it:
// it takes kubernetes as an argument, so it is equally correct under either
// default. Read from a fresh flag set the way runObserve builds one, so flipping
// the registered default fails here rather than silently changing which Cortex a
// bare `agentop observe` connects to.
func TestKubernetesFlag_DefaultsToFalse(t *testing.T) {
	// registerObserveFlags, not a flag set of this test's own: declaring
	// "kubernetes" here with a default of its own choosing would assert that false
	// equals false, and go on passing with production flipped to true — which is the
	// one thing this test exists to catch.
	fs := flag.NewFlagSet("agentop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerObserveFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	kubernetes := f.kubernetes
	if *kubernetes {
		t.Error("--kubernetes must default to false, so a bare `agentop observe` takes a live local Cortex")
	}
	// The flag package's own record of the default, which is what --help prints.
	if got := fs.Lookup("kubernetes").DefValue; got != "false" {
		t.Errorf("registered default = %q, want \"false\"", got)
	}
	// And the default must leave a live local Cortex selected, which is the
	// behaviour the default exists to preserve.
	if got := chooseEndpoint("", "http://localhost:47601", true, *kubernetes); got == "" {
		t.Error("with the default, a live local Cortex must be connected to rather than yielding the picker")
	}
}

// The end-to-end half: nothing above pins that runObserve still CALLS
// registerObserveFlags, so a re-inlined fs.Bool("kubernetes", true, …) would slip
// past every assertion in this file. This one re-execs the real binary and reads its
// --help, which is the user-visible symptom of a flipped default.
//
// Asserting on the absence of "(default true)" rather than the presence of anything:
// flag.PrintDefaults prints "(default X)" only for a non-zero default, so a false
// bool is silent and there is no positive string to match. --kubernetes is the only
// bool in this flag set, so that phrase can come from nothing else.
func TestObserveHelp_DoesNotAdvertiseATrueDefault(t *testing.T) {
	out := runObserveHelp(t)
	if !strings.Contains(out, "-kubernetes") {
		t.Fatalf("--help does not mention -kubernetes, so this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, "(default true)") {
		t.Errorf("--help advertises a true default; --kubernetes must default to false:\n%s", out)
	}
}
