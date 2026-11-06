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
	// Body is the body as the proxy read it, up to MaxResponseCapture bytes: as it came off
	// the wire, in its Content-Encoding, unless Decoded is set.
	Body []byte
	// Decoded is set when the proxy's transport decompressed the body: it asked for the
	// encoding itself, to read the body for a plugin, so the wire bytes never reached the proxy.
	Decoded bool
	// Truncated is set when the body was longer than MaxResponseCapture.
	Truncated bool
	// Incomplete is set when the body did not arrive whole: the stream broke, or the client
	// went away first.
	Incomplete bool
	// Ended is set by the listener when it read the body to its end. Not stored: it decides
	// Incomplete.
	Ended bool
}

// MaxResponseCapture is the most of a response body a capture keeps: the forward proxy's
// buffered-response cap.
const MaxResponseCapture = 10 << 20

// RawCapture is an event's exact capture as GET /v1/sessions/{id}/events/{seq}?raw=1 serves it
// (#1494). A request event's has the request fields, a response event's has Response. A body is
// text when it is valid UTF-8, which every JSON API body is, and base64 in the matching *Base64
// field otherwise, so any body survives the trip.
type RawCapture struct {
	Request         string      `json:"request,omitempty"`
	RequestBase64   []byte      `json:"requestBase64,omitempty"`
	Forwarded       *string     `json:"forwarded,omitempty"`
	ForwardedBase64 []byte      `json:"forwardedBase64,omitempty"`
	RequestHeaders  http.Header `json:"requestHeaders,omitempty"`
	Query           string      `json:"query,omitempty"`
	Response        *RawBody    `json:"response,omitempty"`
}

// RawBody is a captured response. Body is in its Content-Encoding unless Decoded is set, so a
// compressed body is in BodyBase64.
type RawBody struct {
	Status     int         `json:"status"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       string      `json:"body,omitempty"`
	BodyBase64 []byte      `json:"bodyBase64,omitempty"`
	Decoded    bool        `json:"decoded,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
	Incomplete bool        `json:"incomplete,omitempty"`
}
