package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
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
			"v0.9.5", true, "· updated to v0.9.6, restart agentop"},
		{"failed", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultFailed, CheckedAt: noticeNow},
			"v0.9.5", true, "· update to v0.9.6 failed, see agentop doctor"},
		{"available", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.6", Result: updatestatus.ResultWaiting, CheckedAt: noticeNow},
			"v0.9.5", true, "· v0.9.6 available"},
		{"stale", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-73 * time.Hour)},
			"v0.9.5", true, "· no update check since 2026-11-09"},
		{"current", updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-time.Hour)},
			"v0.9.5", true, ""},
	} {
		if got := updateSuffix(tc.st, tc.version, tc.service, noticeNow); got != tc.want {
			t.Errorf("%s: updateSuffix = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestUpdateSuffixIgnoresEmptyLatest(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.5", Result: updatestatus.ResultUpdated, CheckedAt: noticeNow}
	if got := updateSuffix(st, "v0.9.5", true, noticeNow); got != "" {
		t.Errorf("updateSuffix with empty latest = %q, want empty", got)
	}
}

func TestUpdateSuffixDevBuild(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.6", Latest: "v0.9.6", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow}
	if got := updateSuffix(st, "dev-abc", true, noticeNow); got != "" {
		t.Errorf("updateSuffix on a dev build = %q, want empty", got)
	}
}

func TestUpdateSuffixStaleNeedsService(t *testing.T) {
	st := updatestatus.Status{Installed: "v0.9.5", Latest: "v0.9.5", Result: updatestatus.ResultCurrent, CheckedAt: noticeNow.Add(-73 * time.Hour)}
	if got := updateSuffix(st, "v0.9.5", false, noticeNow); got != "" {
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

func TestUpdateNoticeRereadOnReconnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), updatestatus.FileName)
	writeNoticeFile(t, path, "v0.9.6")
	m := &model{version: "v0.9.5", updateFile: path}
	m.readUpdateNotice()
	m.handleStreamEvent(apiclient.StreamEvent{Status: apiclient.StreamStatus{Phase: "reconnecting", Attempt: 1}})
	writeNoticeFile(t, path, "v0.9.7")
	m.handleStreamEvent(apiclient.StreamEvent{Status: apiclient.StreamStatus{Phase: "open"}})
	if got, want := m.updateNote, "· v0.9.7 available"; got != want {
		t.Errorf("updateNote after reconnect = %q, want %q", got, want)
	}
	// An "open" that does not follow a reconnect does not read the file again.
	writeNoticeFile(t, path, "v0.9.8")
	m.handleStreamEvent(apiclient.StreamEvent{Status: apiclient.StreamStatus{Phase: "open"}})
	if got, want := m.updateNote, "· v0.9.7 available"; got != want {
		t.Errorf("updateNote after a second open = %q, want %q", got, want)
	}
}
