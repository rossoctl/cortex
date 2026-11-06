package pipeline

import "net/http"

// RequestCapture is one exchange exactly as it crossed the proxy, for the session archive's
// lossless capture (#1494). On a request event it holds the body the agent sent, the body Cortex
// forwarded, and the request's headers and query. On a response event it holds Response alone.
// The archive stores it beside its event and rebuilds it byte for byte. It never enters the
// in-memory session store.
type RequestCapture struct {
	// Original is the body as the agent sent it.
	Original []byte
	// Forwarded is the body as Cortex sent it upstream, nil when no plugin rewrote it.
	Forwarded []byte
	// Header is the request's headers. The producer removes credential headers before it
	// hands the capture over: the archive stores what it is given.
	Header http.Header
	// Query is the request URL's raw query, without the '?'. The producer removes
	// query-borne credentials too (Gemini's key=, any api_key=): the archive stores the
	// query as given.
	Query string

	// Response is the response, on a response event's capture.
	Response *ResponseCapture
}

// ResponseCapture is a response as the origin sent it.
type ResponseCapture struct {
	Status int
	// Header is the response's headers, without credentials.
	Header http.Header
	// Body is the body as it came off the wire, still in its Content-Encoding, up to
	// MaxResponseCapture bytes.
	Body []byte
	// Truncated is set when the body was longer than MaxResponseCapture.
	Truncated bool
	// Incomplete is set when the body did not arrive whole: the stream broke, or the client
	// went away first.
	Incomplete bool
}

// MaxResponseCapture is the most of a response body a capture keeps: the forward proxy's
// buffered-response cap.
const MaxResponseCapture = 10 << 20
