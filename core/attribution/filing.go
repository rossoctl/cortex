package attribution

import (
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// These rules decide which session a listener files a request under. trusted is the
// listener's own answer from how the connection arrived (vmcontrol), "" for none. It wins
// over every other rule. active is the store's ActiveSession. It is a function so that a
// rule reads it only when it needs it, at the time the row is written (#1187).

// PluginSession is the session handed to plugins and how it was decided: trusted, else
// a.PluginID(), else active() when a names nothing at all, else "". A plugin can hear
// "nothing known" here, so the default bucket is never its session.
func (a Attribution) PluginSession(trusted string, active func() string) (string, model.SessionSource) {
	if trusted != "" {
		return trusted, model.SessionSourceConnection
	}
	if sid := a.PluginID(); sid != "" {
		return sid, a.Source
	}
	if a.Session == "" && a.Source != model.SessionSourcePending {
		if sid := active(); sid != "" {
			return sid, model.SessionSourceActive
		}
	}
	return "", ""
}

// RecordSession is the bucket a request row is recorded under: trusted, else a.RecordID(),
// else active(), else the default bucket. A row must go in a bucket, so it is never "".
func (a Attribution) RecordSession(trusted string, active func() string) string {
	if trusted == "" {
		trusted = a.RecordID()
	}
	return FiledSession(trusted, active)
}

// FiledSession is the bucket for a row of an exchange whose bucket is already pinned: a
// response row, or a tunnel's own row. It is pinned, else active(), else the default bucket.
func FiledSession(pinned string, active func() string) string {
	if pinned != "" {
		return pinned
	}
	if sid := active(); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}
