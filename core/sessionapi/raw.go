package sessionapi

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// eventWithRaw is an event with its exact capture beside it, the answer to ?raw=1: the event's own
// fields, as GET without the parameter serves them, and "raw".
type eventWithRaw struct {
	*pipeline.SessionEvent
	Raw *pipeline.RawCapture
}

func (e eventWithRaw) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(e.SessionEvent)
	if err != nil || len(b) < 2 {
		return b, err
	}
	r, err := json.Marshal(e.Raw)
	if err != nil {
		return nil, err
	}
	// SessionEvent has its own MarshalJSON, so the key is spliced into its object rather than
	// declared beside it.
	return append(append(append(b[:len(b)-1:len(b)-1], `,"raw":`...), r...), '}'), nil
}

func rawOf(c *pipeline.RequestCapture) *pipeline.RawCapture {
	r := &pipeline.RawCapture{RequestHeaders: c.Header, Query: c.Query}
	r.Request, r.RequestBase64 = textOr64(c.Original)
	if c.Forwarded != nil {
		f, f64 := textOr64(c.Forwarded)
		if f64 == nil {
			r.Forwarded = &f
		}
		r.ForwardedBase64 = f64
	}
	if p := c.Response; p != nil {
		r.Response = &pipeline.RawBody{Status: p.Status, Headers: p.Header, Decoded: p.Decoded, Truncated: p.Truncated, Incomplete: p.Incomplete}
		r.Response.Body, r.Response.BodyBase64 = textOr64(p.Body)
	}
	return r
}

func textOr64(b []byte) (string, []byte) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	return "", b
}
