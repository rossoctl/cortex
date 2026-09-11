package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// End-to-end through the real file: save what the TUI would produce, then load it
// back the way main does at startup. This is the loop the feature exists for, and
// no other test spans both packages.
func TestEndToEnd_SettingsSurviveARestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := userConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cortex", "abctl-config.yaml"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	// --- "first run": nothing on disk, defaults, no complaint.
	var warn bytes.Buffer
	tui.Settings = loadUserConfig(path, &warn)
	if warn.Len() != 0 {
		t.Errorf("first run warned: %q", warn.String())
	}

	// --- the user turns COST off and commits a filter, as the key handlers do.
	tui.Settings.Events.Columns = []tui.ColumnSetting{{Name: "COST", Visible: false}}
	tui.Settings.Filter = "github-tool"
	if err := saveUserConfig(path, tui.Settings); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("file on disk:\n%s", body)

	// --- "restart": a fresh process loads it.
	tui.Settings = tui.UserSettings{}
	warn.Reset()
	tui.Settings = loadUserConfig(path, &warn)
	if warn.Len() != 0 {
		t.Errorf("reload warned: %q", warn.String())
	}
	if tui.Settings.Filter != "github-tool" {
		t.Errorf("filter = %q after restart, want it remembered", tui.Settings.Filter)
	}
	if len(tui.Settings.Events.Columns) != 1 || tui.Settings.Events.Columns[0].Name != "COST" {
		t.Errorf("columns = %+v after restart", tui.Settings.Events.Columns)
	}
}
