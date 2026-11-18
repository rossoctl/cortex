package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveServicePaths_SurvivesAMissingBinary: uninstall and status are what you
// reach for when things are broken, including when the proxy binary is gone.
// Refusing to resolve paths then would leave a loaded unit with no way to remove it.
func TestResolveServicePaths_SurvivesAMissingBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "nowhere")) // no cortex anywhere
	cfgDir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := resolveServicePaths(cfg, "", "")
	if err != nil {
		t.Fatalf("resolveServicePaths failed with no binary present: %v\n"+
			"uninstall and status must still work in that state", err)
	}
	if p.unitFile == "" {
		t.Error("no unit path resolved, so uninstall would have nothing to remove")
	}
}

// TestServiceInstall_RefusesABrokenConfig: without this, install writes the unit,
// gets an empty healthURL, skips the probe, and calls a proxy that cannot start
// "running".
func TestServiceInstall_RefusesABrokenConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	// Invalid YAML: an unterminated flow sequence.
	if err := os.WriteFile(cfg, []byte("listener:\n  roles: [forward\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A binary that exists, so this isolates the config check.
	bin := filepath.Join(home, "cortex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	p, err := resolveServicePaths(cfg, filepath.Join(home, "unit"), "")
	if err != nil {
		t.Fatal(err)
	}
	p.binary = bin
	if p.configErr == nil {
		t.Fatal("an invalid config did not record a load error")
	}

	var out, errOut bytes.Buffer
	if code := serviceInstall(p, true, false, &out, &errOut); code == 0 {
		t.Error("install succeeded on a config that cannot load")
	}
	if !strings.Contains(errOut.String(), "will not load") {
		t.Errorf("the reason was not reported: %s", errOut.String())
	}
	if _, serr := os.Stat(p.unitFile); serr == nil {
		t.Error("a unit was written for a config that cannot load")
	}
}

// TestCrashRecoveryNote checks the message itself, on every platform rather than only
// on darwin: the const exists so these assertions are not skipped on CI.
func TestCrashRecoveryNote(t *testing.T) {
	if !strings.Contains(crashRecoveryNote, "supervisor") {
		t.Error("note does not name the supervisor; two cortex processes " +
			"with no explanation is the first thing people ask about")
	}
	if strings.Contains(strings.TrimSpace(crashRecoveryNote), "\n") {
		t.Errorf("note is more than one line; keep it short:\n%s", crashRecoveryNote)
	}
}
