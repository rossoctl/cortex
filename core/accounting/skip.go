package accounting

import "strconv"

// SkipInput is what the host knows about a finished request, before it decides whether to call
// accountant.Request.Compute (accountant/docs/cortex-wiring.md W6.1).
type SkipInput struct {
	// Status is the upstream response status. 0 means no provider answered (for example, a
	// request the proxy answered itself, such as /compact): that is not a rejection.
	Status int
	// Rounds is the number of upstream round trips this one client request caused. More than one
	// means a single usage object does not describe a single forwarded body.
	Rounds int
	// SessionID is empty when the request has no session: the thread key (design §5.4) needs one.
	SessionID string
	// ServerHeldHistory is true for an OpenAI Responses request whose prompt history the
	// provider holds (previous_response_id, conversation, or an item_reference input item): the
	// body this host sees holds only the new turn, and the library would read the missing
	// history as a client edit (W6.1, #899).
	ServerHeldHistory bool
	// UnplacedDeferral is true when a tool deferral (or similar body rewrite) could not be
	// described to the library: computing on the bodies as they are would fail the library's sum
	// check (W6.1).
	UnplacedDeferral bool
}

// SkipReason says why Compute must not be called for this request, or "" to say it should be
// called. The strings are the context-guru proxy's own fallback-reason strings
// (proxy/dashcapture.go: noResultWhy, accDeferralUnplaced), not the shorter names
// accountant/docs/cortex-wiring.md's W6.1 table uses in prose, so that a reader who greps a
// Cortex log line for one of these reasons finds the same string in the proxy's.
//
// Order matters: a request with no session never opened an accounting record at all (the proxy's
// "no_record" case, c.accReq == nil) regardless of its status, and a server-held-history request
// is skipped for its shape before its response is even known, before either upstream-rejection
// reason.
func SkipReason(in SkipInput) string {
	switch {
	case in.SessionID == "":
		return "no_record"
	case in.ServerHeldHistory:
		return "server_held_history"
	case rejected(in.Status):
		return "rejected_status"
	case in.UnplacedDeferral:
		return "deferral_unplaced"
	case in.Rounds > 1:
		return "upstream_rounds=" + strconv.Itoa(in.Rounds)
	}
	return ""
}

// rejected is the proxy's own rule (capture.rejected): a status that is not 0 and not 2xx. Status
// 0 means no provider was reached, which is not a rejection.
func rejected(status int) bool {
	return status != 0 && (status < 200 || status > 299)
}
