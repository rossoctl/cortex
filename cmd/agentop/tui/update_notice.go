package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
)

// updateSuffix is the update notice after the build on the title bar, or "", and
// its ink: green for news, red for a failure, amber for a missed check.
// The first rule that matches wins.
func updateSuffix(st updatestatus.Status, version string, serviceInstall bool, now time.Time) (string, lipgloss.Style) {
	switch {
	case st.Installed != version && updatestatus.IsRelease(st.Installed) &&
		(updatestatus.IsRelease(version) || updatestatus.IsRC(version)):
		// Not "restart agentop": that reads as agentop service restart, which this does not need.
		return "· " + st.Installed + " installed, quit and reopen agentop", styleOK
	case st.Note != "":
		return "· " + st.Note, styleOK
	case st.Result == updatestatus.ResultFailed:
		return "· update to " + st.Latest + " failed, see agentop doctor", styleError
	// Only a newer release: a dev, main or RC build is often ahead of Latest.
	case updatestatus.Newer(st.Latest, st.Installed):
		return "· " + st.Latest + " available", styleOK
	case serviceInstall && now.Sub(st.CheckedAt) > 72*time.Hour:
		return "· no update check since " + st.CheckedAt.Format("2006-01-02"), styleWarn
	}
	return "", styleNone
}

// readUpdateNotice sets m.updateNote from m.updateFile. A missing or bad file
// shows no notice.
func (m *model) readUpdateNotice() {
	m.updateNote = ""
	if m.updateFile == "" {
		return
	}
	st, err := updatestatus.Read(m.updateFile)
	if err != nil {
		return
	}
	m.updateNote, m.updateInk = updateSuffix(st, m.version, m.serviceInstall, time.Now())
}

// titleView is renderTitle with the update notice in its own ink, so it stands out.
func (m *model) titleView(title string) string {
	return renderTitleInk(title, strings.TrimPrefix(m.updateNote, "· "), m.updateInk)
}
