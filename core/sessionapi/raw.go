package sessionapi

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// eventResponse is the answer to GET /v1/sessions/{id}/events/{seq}: the event's own fields,
// "schemaVersion", and, for ?raw=1 when a capture exists, its exact capture under "raw".
type eventResponse struct {
	*pipeline.SessionEvent
	Raw *pipeline.RawCapture
}

func (e eventResponse) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(e.SessionEvent)
	if err != nil || len(b) < 2 {
		return b, err
	}
	// SessionEvent has its own MarshalJSON, so the keys are spliced into its object rather than
	// declared beside it.
	b = append(b[:len(b)-1:len(b)-1], `,"schemaVersion":`+strconv.Itoa(SchemaVersion)...)
	if e.Raw != nil {
		r, err := json.Marshal(e.Raw)
		if err != nil {
			return nil, err
		}
		b = append(append(b, `,"raw":`...), r...)
	}
	return append(b, '}'), nil
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
	r.Translated, r.TranslatedBase64 = optionalTextOr64(c.Translated)
	if p := c.Response; p != nil {
		r.Response = &pipeline.RawBody{Status: p.Status, Headers: p.Header, Decoded: p.Decoded, Truncated: p.Truncated,
			Incomplete: p.Incomplete, UpstreamTruncated: p.UpstreamTruncated}
		r.Response.Body, r.Response.BodyBase64 = textOr64(p.Body)
		r.Response.Upstream, r.Response.UpstreamBase64 = optionalTextOr64(p.Upstream)
	}
	return r
}

// optionalTextOr64 is textOr64 for a body that can be absent: nil and nil for a nil body.
func optionalTextOr64(b []byte) (*string, []byte) {
	if b == nil {
		return nil, nil
	}
	t, t64 := textOr64(b)
	if t64 != nil {
		return nil, t64
	}
	return &t, nil
}

func textOr64(b []byte) (string, []byte) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	return "", b
}
