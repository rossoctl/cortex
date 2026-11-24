//go:build !include_driver_moca

package main

import "testing"

// A build without the moca driver leaves the channel off whatever the
// environment says, rather than refusing a setting it cannot serve.
func TestResolveControlConfigIgnoresMocaEnv(t *testing.T) {
	cc, err := resolveControlConfig(func(string) string { return "/run/c.sock" })
	if err != nil || cc.Socket != "" {
		t.Fatalf("got %+v, %v; want the channel off and no error", cc, err)
	}
}
