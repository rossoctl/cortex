package pipeline

import "net/http"

// RequestCapture is one request exactly as it crossed the proxy, for the session archive's
// lossless capture (#1494): the body the agent sent, the body Cortex forwarded, and the
// request's headers and query. The archive stores it beside the request's event and rebuilds
// it byte for byte. It never enters the in-memory session store.
type RequestCapture struct {
	// Original is the body as the agent sent it.
	Original []byte
	// Forwarded is the body as Cortex sent it upstream, nil when no plugin rewrote it.
	Forwarded []byte
	// Header is the request's headers. The producer removes credential headers before it
	// hands the capture over: the archive stores what it is given.
	Header http.Header
	// Query is the request URL's raw query, without the '?'.
	Query string
}
