package main

import (
	"strings"
	"testing"
)

// agentop and install.sh take the last word of `cortex --version` as the
// version, so a build variant must never be the last word.
func TestVersionLine(t *testing.T) {
	old := version
	version = "v9.9.9"
	t.Cleanup(func() { version = old })

	got := versionLine()
	fields := strings.Fields(got)
	if fields[len(fields)-1] != version {
		t.Fatalf("last field of %q = %q, want %q", got, fields[len(fields)-1], version)
	}
	want, ok := map[string]string{
		"":       "cortex v9.9.9",
		"(cpex)": "cortex (cpex) v9.9.9",
	}[variant]
	if !ok {
		t.Fatalf("unknown build variant %q", variant)
	}
	if got != want {
		t.Fatalf("versionLine() = %q, want %q", got, want)
	}
}
