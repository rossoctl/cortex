package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
)

var noticeNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

func TestUpdateSuffix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      updatestatus.Status
		version string
		service bool
		want    string
	}{
		{"updated", updatestatus.Status{Installed: "v0.9.6", Latest: "v0.9.6", Result: updatestatus.ResultUpdated, CheckedAt: noticeNow},
			"v0.9.5", true, "· v0.9.6 installed, quit and reopen agentop"},
		{"failed", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultFailed, CheckedAt: noticeNow},
			"v0.9.5", true, "· update to v0.9.6 failed, see agentop doctor"},
		// A cpex replacement keeps the version, so the only sign is the note.
		{"cpex note", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultUpdated, Note: "the laptop cpex build is removed", CheckedAt: noticeNow},
			"v0.9.5", true, "· the laptop cpex build is removed"},
		{"available", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultWaiting, CheckedAt: noticeNow},
			"v0.9.5", true, "· v0.9.6 available"},
		{"stale", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-73 * time.Hour)},
			"v0.9.5", true, "· no update check since 2026-11-09"},
		{"current", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-time.Hour)},
			"v0.9.5", true, ""},
		// A dev build has no order against Latest, and is often ahead of it.
		{"dev build", updatestatus.Status{Installed: "dev", Latest: "v0.9.5", Result: updatestatus.ResultNotARelease, CheckedAt: noticeNow},
			"dev-a698b60e+dirty", true, ""},
		{"rc ahead", updatestatus.Status{Installed: "v0.10.0-rc.1", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow},
			"v0.10.0-rc.1", true, ""},
		{"rc release out", updatestatus.Status{Installed: "v0.10.0-rc.1", Latest: "v0.10.0", Result: updatestatus.ResultWaiting, CheckedAt: noticeNow},
			"v0.10.0-rc.1", true, "· v0.10.0 available"},
		{"rc updated", updatestatus.Status{Installed: "v0.10.0", Latest: "v0.10.0", Result: updatestatus.ResultUpdated, CheckedAt: noticeNow},
			"v0.10.0-rc.1", true, "· v0.10.0 installed, quit and reopen agentop"},
		// Latest moved back: the update installs it, with no "available" before.
		{"withdrawn", updatestatus.Status{Installed: "v0.10.0", Latest: "v0.9.5", Result: updatestatus.ResultWaiting, CheckedAt: noticeNow},
			"v0.10.0", true, ""},
	} {
		if got, _ := updateSuffix(tc.st, tc.version, tc.service, noticeNow); got != tc.want {
			t.Errorf("%s: updateSuffix = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestUpdateSuffixIgnoresEmptyLatest(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.5", Result: updatestatus.ResultUpdated, CheckedAt: noticeNow}
	if got, _ := updateSuffix(st, "v0.9.5", true, noticeNow); got != "" {
		t.Errorf("updateSuffix with empty latest = %q, want empty", got)
	}
}

func TestUpdateSuffixDevBuild(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.6", Latest: "v0.9.6", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow}
	if got, _ := updateSuffix(st, "dev-abc", true, noticeNow); got != "" {
		t.Errorf("updateSuffix on a dev build = %q, want empty", got)
	}
}

func TestUpdateSuffixStaleNeedsService(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-73 * time.Hour)}
	if got, _ := updateSuffix(st, "v0.9.5", false, noticeNow); got != "" {
		t.Errorf("updateSuffix with no service = %q, want empty", got)
	}
}

func writeNoticeFile(t *testing.T, path, latest string) {
	t.Helper()
	body := `{"checkedAt":"` + time.Now().UTC().Format(time.RFC3339) + `","latest":"` + latest + `","installed":"v0.9.5","result":"waiting","error":""}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAppNameCarriesUpdateNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), updatestatus.FileName)
	writeNoticeFile(t, path, "v0.9.6")
	m := &model{version: "v0.9.5", updateFile: path}
	m.readUpdateNotice()
	if got, want := m.appName(), "agentop v0.9.5 · v0.9.6 available"; got != want {
		t.Errorf("appName = %q, want %q", got, want)
	}
}

// The updater writes the file after the restarted proxy serves, so the stream is
// often open again before the result lands: each refresh tick reads it.
func TestUpdateNoticeRereadOnRefreshTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), updatestatus.FileName)
	writeNoticeFile(t, path, "v0.9.6")
	m := &model{version: "v0.9.5", updateFile: path, pane: paneNamespaces, pickerShowing: true, pickerHarvested: true}
	m.readUpdateNotice()
	writeNoticeFile(t, path, "v0.9.7")
	m.update(refreshTickMsg(time.Now()))
	if got, want := m.updateNote, "· v0.9.7 available"; got != want {
		t.Errorf("updateNote after a tick = %q, want %q", got, want)
	}
}

// Green for news, red for a failure, amber for a missed check.
func TestUpdateSuffixInk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      updatestatus.Status
		version string
		want    lipgloss.Style
	}{
		{"installed", updatestatus.Status{Installed: "v0.9.6", Latest: "v0.9.6", Result: updatestatus.ResultUpdated, CheckedAt: noticeNow}, "v0.9.5", styleOK},
		{"note", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultUpdated, Note: "n", CheckedAt: noticeNow}, "v0.9.5", styleOK},
		{"failed", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultFailed, CheckedAt: noticeNow}, "v0.9.5", styleError},
		{"available", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultWaiting, CheckedAt: noticeNow}, "v0.9.5", styleOK},
		{"stale", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-73 * time.Hour)}, "v0.9.5", styleWarn},
	} {
		_, ink := updateSuffix(tc.st, tc.version, true, noticeNow)
		if got, want := ink.Render("x"), tc.want.Render("x"); got != want {
			t.Errorf("%s: ink renders %q, want %q", tc.name, got, want)
		}
	}
}

// The title keeps its text, and only the notice takes the notice's ink.
func TestTitleViewInksTheNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), updatestatus.FileName)
	writeNoticeFile(t, path, "v0.9.6")
	m := &model{version: "v0.9.5", updateFile: path}
	m.readUpdateNotice()
	withProfile(t, termenv.TrueColor)
	in := m.appName() + " · http://127.0.0.1:47601"
	got := m.titleView(in)
	if s := ansi.Strip(got); s != in {
		t.Fatalf("stripped = %q, want %q", s, in)
	}
	if want := styleOK.Bold(true).Render("v0.9.6 available"); !strings.Contains(got, want) {
		t.Errorf("notice not green bold in %q", got)
	}
	if !strings.Contains(got, styleMuted.Render("http://127.0.0.1:47601")) {
		t.Errorf("URL lost its style in %q", got)
	}
	// With no notice, titleView is renderTitle.
	m.updateNote, m.updateInk = "", styleNone
	if got, want := m.titleView(in), renderTitle(in); got != want {
		t.Errorf("titleView with no notice = %q, want %q", got, want)
	}
}
