package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Unset follows the caller's default; an explicit false wins over it. The default is decided
// by the binary — on for a local install once the archive launches — and the config must not
// be able to say "on" by omission anywhere else.
func TestSessionArchiveConfig_UnsetFollowsTheDefaultAndFalseWins(t *testing.T) {
	var unset *SessionArchiveConfig
	if !unset.ArchiveEnabled(true) || unset.ArchiveEnabled(false) {
		t.Fatal("a nil block must follow the default")
	}
	off := false
	if (&SessionArchiveConfig{Enabled: &off}).ArchiveEnabled(true) {
		t.Fatal("enabled: false must win over a default of on")
	}
}

func TestSessionArchiveConfig_Validate(t *testing.T) {
	for _, tc := range []struct {
		cfg  SessionArchiveConfig
		want string // "" means valid
	}{
		{SessionArchiveConfig{}, ""},
		{SessionArchiveConfig{RetentionDays: 30, MaxBytes: 1 << 30}, ""},
		{SessionArchiveConfig{RetentionDays: -1}, "session.archive.retention_days"},
		{SessionArchiveConfig{RetentionDays: 3651}, "session.archive.retention_days"},
		{SessionArchiveConfig{MaxBytes: -1}, "session.archive.max_bytes"},
		{SessionArchiveConfig{Dir: "/var/lib/cortex/sessions"}, ""},
		{SessionArchiveConfig{Dir: "sessions"}, "session.archive.dir"},
	} {
		err := tc.cfg.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: unexpected error %v", tc.cfg, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: error %v, want one naming %s", tc.cfg, err, tc.want)
		}
	}
}

// The archive records through the session store, so with the store off it can never see an
// event. Saying both explicitly is a contradiction, refused at load like the cost ledger's.
func TestValidate_RefusesTheArchiveWithoutSessions(t *testing.T) {
	on, off := true, false
	c := &Config{
		Mode:     ModeProxySidecar,
		Listener: forwardOnlyListener(),
		Session:  SessionConfig{Enabled: &off, Archive: &SessionArchiveConfig{Enabled: &on}},
	}
	err := Validate(c)
	if err == nil {
		t.Fatal("session.archive.enabled: true with session.enabled: false accepted")
	}
	for _, want := range []string{"session.archive.enabled", "session.enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	c.Session.Archive.Enabled = nil // the default case is the binary's to warn about, not a load error
	if err := Validate(c); err != nil {
		t.Fatalf("an unset archive with sessions off was refused: %v", err)
	}
}

func TestLoad_ReadsTheSessionArchiveBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "mode: proxy-sidecar\nsession:\n  archive:\n    enabled: true\n    retention_days: 14\n    max_bytes: 1073741824\n    dir: /var/lib/cortex/sessions\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Session.Archive
	if a == nil || a.Enabled == nil || !*a.Enabled || a.RetentionDays != 14 || a.MaxBytes != 1<<30 ||
		a.Dir != "/var/lib/cortex/sessions" {
		t.Fatalf("session.archive = %+v", a)
	}
}

func TestLoad_RefusesAnInvalidSessionArchiveBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: proxy-sidecar\nsession:\n  archive:\n    max_bytes: -5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("Load = %v, want an error naming max_bytes", err)
	}
}

// The archive's rule on a local install: false always wins, true opts in, and unset follows
// listener.bind_loopback_only — the condition DELETE /v1/sessions needs to clear it.
func TestArchiveRunsOnLocalInstall(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name     string
		enabled  *bool
		block    bool
		loopback bool
		want     bool
		reason   string
	}{
		{"no block, loopback only", nil, false, true, true, "on by default"},
		{"no block, not loopback only", nil, false, false, false, "bind_loopback_only is false"},
		{"block without enabled, loopback only", nil, true, true, true, "on by default"},
		{"block without enabled, not loopback only", nil, true, false, false, "bind_loopback_only is false"},
		{"enabled, loopback only", &on, true, true, true, "enabled is true"},
		{"enabled, not loopback only", &on, true, false, true, "refuses to clear"},
		{"disabled, loopback only", &off, true, true, false, "enabled is false"},
		{"disabled, not loopback only", &off, true, false, false, "enabled is false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			c.Listener.BindLoopbackOnly = tc.loopback
			if tc.block {
				c.Session.Archive = &SessionArchiveConfig{Enabled: tc.enabled}
			}
			got, why := c.ArchiveRunsOnLocalInstall()
			if got != tc.want || !strings.Contains(why, tc.reason) {
				t.Errorf("ArchiveRunsOnLocalInstall = %v (%q), want %v and a reason naming %q", got, why, tc.want, tc.reason)
			}
		})
	}
}
