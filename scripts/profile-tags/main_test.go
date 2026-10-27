package main

import "testing"

// TestRun_PrintsCSV — call sites embed this in `go build -tags "$(...)"`, so the
// output must be a bare comma-separated list with no surrounding noise.
func TestRun_PrintsCSV(t *testing.T) {
	got, err := run([]string{"local"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "include_plugin_a2aparser,include_plugin_inferenceparser," +
		"include_plugin_mcpparser,include_plugin_script,include_plugin_toolprune"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRun_RequiresProfile — a call site that forgets the argument must fail
// loudly. Printing an empty string would flow into `go build -tags ""`, which
// under all-opt-in links no plugins and silently ships an inert binary.
func TestRun_RequiresProfile(t *testing.T) {
	if _, err := run(nil); err == nil {
		t.Fatal("want error when no profile is given, got nil")
	}
}
