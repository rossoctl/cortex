package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateAutoEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"no block", "mode: proxy-sidecar\n", true},
		{"auto true", "mode: proxy-sidecar\nupdate:\n  auto: true\n", true},
		{"auto false", "mode: proxy-sidecar\nupdate:\n  auto: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.UpdateAutoEnabled(); got != tc.want {
				t.Fatalf("UpdateAutoEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
	if !(*Config)(nil).UpdateAutoEnabled() {
		t.Fatal("(*Config)(nil).UpdateAutoEnabled() = false, want true")
	}
}

func TestUpdateTimes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		u           *UpdateConfig
		wait, every time.Duration
		bad         bool
	}{
		{"nil", nil, DefaultUpdateWait, DefaultUpdateCheckEvery, false},
		{"unset", &UpdateConfig{}, DefaultUpdateWait, DefaultUpdateCheckEvery, false},
		{"set", &UpdateConfig{Wait: "72h", CheckEvery: "24h"}, 72 * time.Hour, 24 * time.Hour, false},
		{"wait 0", &UpdateConfig{Wait: "0"}, 0, DefaultUpdateCheckEvery, false},
		{"check 1h", &UpdateConfig{CheckEvery: "1h"}, DefaultUpdateWait, time.Hour, false},
		{"wait bad", &UpdateConfig{Wait: "soon"}, DefaultUpdateWait, DefaultUpdateCheckEvery, true},
		{"wait negative", &UpdateConfig{Wait: "-1h"}, DefaultUpdateWait, DefaultUpdateCheckEvery, true},
		{"check too short", &UpdateConfig{CheckEvery: "59m"}, DefaultUpdateWait, DefaultUpdateCheckEvery, true},
		{"check too long", &UpdateConfig{Wait: "1h", CheckEvery: "25h"}, DefaultUpdateWait, DefaultUpdateCheckEvery, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wait, every, err := tc.u.Times()
			if wait != tc.wait || every != tc.every || (err != nil) != tc.bad {
				t.Fatalf("Times() = %v, %v, %v; want %v, %v, error %v", wait, every, err, tc.wait, tc.every, tc.bad)
			}
		})
	}
}

func TestValidateRefusesBadUpdateTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: proxy-sidecar\nupdate:\n  check_every: 10m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listener.ReverseProxyBackend = "http://127.0.0.1:1" // the one field proxy-sidecar needs
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "update.check_every") {
		t.Fatalf("Validate() = %v, want an update.check_every error", err)
	}
}
