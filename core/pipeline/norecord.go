package pipeline

import "github.com/rossoctl/cortex/core/model"

// NoRecord reports whether the request asked not to be recorded (#1923): it carried
// model.RecordHeader set to model.RecordOff. With an own session id (WithholdsSession), it is
// recorded as SessionEvent.NoRecord on the request's rows, and the archive keeps none of that
// session's events from then on; what it held before stays.
//
// The first call also deletes model.RecordHeader from Headers. The header is for Cortex
// only: plugins never see it, and the listeners that send the request on from Headers
// (httpx.SyncRequestHeaders) do not forward it. ResolveClient makes that first call at
// construction, before any plugin runs, so a plugin cannot set or clear it either.
//
// Memoized and pinned by ResolveClient on exactly the terms ClientInfo is; see there.
//
// CLIENT-ASSERTED AND UNAUTHENTICATED, like Subagent. It can only withhold the sender's own
// session from the archive.
func (c *Context) NoRecord() bool {
	if c.noRecordParsed {
		return c.noRecord
	}
	c.noRecordParsed = true
	if c.Headers == nil {
		return false
	}
	c.noRecord = model.RecordsOff(c.Headers)
	c.recordHeaderSeen = len(c.Headers.Values(model.RecordHeader)) > 0
	c.Headers.Del(model.RecordHeader)
	return c.noRecord
}

// RecordHeaderSeen reports whether the client sent model.RecordHeader, with any value. NoRecord
// removed it from Headers, which is enough for a listener that forwards from Headers. One that
// forwards the wire headers and emits changes (ext_proc) must remove it explicitly.
func (c *Context) RecordHeaderSeen() bool {
	_ = c.NoRecord()
	return c.recordHeaderSeen
}

// WithholdsSession reports whether this request's session is to be withheld from the archive:
// it asked not to be recorded (NoRecord) AND it names its own session with a session-id header
// (NamingHeader, pinned by SetAttribution). This is what listeners record as
// SessionEvent.NoRecord.
//
// THE OWN-ID RULE IS THE SAFETY. A request that names no session is filed by fallback, to the
// active session or the shared default bucket (attribution.FiledSession), so honouring its flag
// would stop recording a session that is not the sender's. Such a request is recorded as if it
// had not asked; its header is still stripped. A request whose header names another session can
// still stop that session's recording from then on, but never erase what is already archived:
// the session-id header is client-asserted, with session.IDFromHeaders' TRUST caveat.
func (c *Context) WithholdsSession() bool {
	return c.NoRecord() && c.namingHeader != ""
}
