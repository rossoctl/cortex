package tui

import (
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/updatestatus"
)

// updateSuffix is the update notice after the build on the title bar, or "".
// The first rule that matches wins.
func updateSuffix(st updatestatus.Status, version string, serviceInstall bool, now time.Time) string {
	switch {
	case st.Installed != version && updatestatus.IsRelease(st.Installed) &&
		(updatestatus.IsRelease(version) || updatestatus.IsRC(version)):
		return "· updated to " + st.Installed + ", restart agentop"
	case st.Note != "":
		return "· " + st.Note
	case st.Result == updatestatus.ResultFailed:
		return "· update to " + st.Latest + " failed, see agentop doctor"
	// Only a newer release: a dev, main or RC build is often ahead of Latest.
	case updatestatus.Newer(st.Latest, st.Installed):
		return "· " + st.Latest + " available"
	case serviceInstall && now.Sub(st.CheckedAt) > 72*time.Hour:
		return "· no update check since " + st.CheckedAt.Format("2006-01-02")
	}
	return ""
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
	m.updateNote = updateSuffix(st, m.version, m.serviceInstall, time.Now())
}
