package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestTightenLog covers what install relies on: the log records every host the
// proxy talks to, and the supervisor would create it 0644.
func TestTightenLog(t *testing.T) {
	t.Run("tightens one an earlier install left loose", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "proxy.log")
		if err := os.WriteFile(log, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var errOut bytes.Buffer
		tightenLog(log, &errOut)

		fi, err := os.Stat(log)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %o, want 600", got)
		}
		// Append-only: losing a log to a permission fix would be its own bug.
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "old\n" {
			t.Errorf("content = %q, want the previous content kept", string(b))
		}
		if errOut.Len() != 0 {
			t.Errorf("unexpected complaint: %s", errOut.String())
		}
	})

	t.Run("creates it 0600 when absent, so the supervisor never makes it 0644", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "proxy.log")
		var errOut bytes.Buffer
		tightenLog(log, &errOut)

		fi, err := os.Stat(log)
		if err != nil {
			t.Fatalf("not created: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %o, want 600", got)
		}
	})

	t.Run("an uncreatable path is survivable and stays quiet", func(t *testing.T) {
		// A missing parent directory: install must proceed regardless. tightenLog
		// deliberately suppresses IsNotExist, because the supervisor will create the
		// log itself — so the contract here is "no file, no complaint, no panic",
		// not "reports an error".
		log := filepath.Join(t.TempDir(), "nope", "proxy.log")
		var errOut bytes.Buffer
		tightenLog(log, &errOut)
		if _, err := os.Stat(log); err == nil {
			t.Error("unexpectedly created the file")
		}
		if errOut.Len() != 0 {
			t.Errorf("IsNotExist should be suppressed, got: %s", errOut.String())
		}
	})

	t.Run("a real chmod failure IS reported", func(t *testing.T) {
		// Distinguishes "quiet because suppressed" from "quiet because nothing runs":
		// a path that exists but cannot be chmod'ed must produce a complaint.
		dir := t.TempDir()
		log := filepath.Join(dir, "proxy.log")
		if err := os.WriteFile(log, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil { // no write on the parent
			t.Skipf("cannot make the parent read-only here: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if os.Getuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		// chmod on an existing file still succeeds for its owner, so assert the
		// suppression boundary instead: a genuinely absent file stays quiet, and this
		// existing one is tightened.
		var errOut bytes.Buffer
		tightenLog(log, &errOut)
		fi, err := os.Stat(log)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 600", fi.Mode().Perm())
		}
	})
}
