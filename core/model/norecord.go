package model

import (
	"net/http"
	"strings"
)

// RecordHeader is the request header a client sends to keep its session out of the
// session archive (#1923). RecordOff is the one value that does so.
//
// For Cortex only: a listener reads it before any plugin runs and removes it from the
// request it forwards, so the upstream never sees it. Named like the other X-Cortex-*
// headers. It is not a session-id header and is never in session.id_headers.
//
// Claude Code sends it from ANTHROPIC_CUSTOM_HEADERS, e.g.
// ANTHROPIC_CUSTOM_HEADERS="X-Cortex-Record: off".
const RecordHeader = "X-Cortex-Record"

// RecordOff is the value of RecordHeader that withholds a session from the archive.
const RecordOff = "off"

// RecordsOff reports whether h asks not to be recorded: RecordHeader is present with the
// value RecordOff, compared without case and padding. Any other value, or none, records as
// today, so a typo can never hide a session by accident. Several values: any one "off" counts,
// because the header can only withhold.
func RecordsOff(h http.Header) bool {
	if h == nil {
		return false
	}
	for _, v := range h.Values(RecordHeader) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), RecordOff) {
				return true
			}
		}
	}
	return false
}
