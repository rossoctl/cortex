//go:build !include_driver_moca

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A build without the moca driver leaves the channel off whatever the
// environment says, rather than refusing a setting it cannot serve, and says so.
func TestResolveControlConfigIgnoresMocaEnv(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	cc, err := resolveControlConfig(func(string) string { return "/run/c.sock" })
	if err != nil || cc.Socket != "" {
		t.Fatalf("got %+v, %v; want the channel off and no error", cc, err)
	}
	if !strings.Contains(buf.String(), "hostegress") {
		t.Fatalf("no warning naming the hostegress profile; log: %q", buf.String())
	}
}
