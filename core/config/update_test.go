package config

import (
	"os"
	"path/filepath"
	"testing"
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
