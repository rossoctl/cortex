package translate

import (
	"fmt"
	"net/http"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// Converted is a request converted to the target protocol.
type Converted struct {
	Body    []byte
	Dropped []string // the fields and blocks that the target could not carry (rule H4)
	Stream  bool     // the agent asked for a stream (decision 4)
	// Req is the decoded request for the reply encoder (rule R2), with no
	// System and no Messages, so the history is not kept twice.
	Req pivot.Request
}

// Request converts a request body from the source protocol to the target
// protocol, and removes the source protocol's own headers from h. A
// refusal to send to the agent is a *pivot.RequestError.
func Request(source, target string, body []byte, h http.Header) (Converted, error) {
	dec, ok := protocols[source].(pivot.RequestDecoder)
	if !ok {
		return Converted{}, fmt.Errorf("translate: protocol %q cannot decode a request", source)
	}
	enc, ok := protocols[target].(pivot.RequestEncoder)
	if !ok {
		return Converted{}, fmt.Errorf("translate: protocol %q cannot encode a request", target)
	}
	req, err := dec.DecodeRequest(body)
	if err != nil {
		return Converted{}, err
	}
	if r, ok := protocols[source].(pivot.SourceHeaderRemover); ok && h != nil {
		r.RemoveSourceHeaders(h)
	}
	out, dropped, err := enc.EncodeRequest(req, h)
	if err != nil {
		return Converted{}, err
	}
	req.System, req.Messages = nil, nil
	return Converted{Body: out, Dropped: dropped, Stream: req.Stream, Req: req}, nil
}

// replyCodecs gives the decoder of the target's reply and the encoder of
// the source's reply. false when a direction is missing.
func replyCodecs(source, target string, req pivot.Request, status int, upstreamStream, agentStream bool) (pivot.ReplyDecoder, pivot.ReplyEncoder, bool) {
	dec, ok := protocols[target].(pivot.ReplyDecoding)
	if !ok {
		return nil, nil, false
	}
	enc, ok := protocols[source].(pivot.ReplyEncoding)
	if !ok {
		return nil, nil, false
	}
	return dec.NewReplyDecoder(status, upstreamStream), enc.NewReplyEncoder(req, agentStream), true
}

// Reply converts a whole reply body of the target protocol to the reply
// that the source protocol's agent expects. req is the decoded request
// (Converted.Req). upstreamStream tells if the
// body is SSE; agentStream tells if the agent asked for a stream. When a
// direction is missing, it gives a 502 error body in the source protocol
// (nil for an unknown source). It also gives the status of the converted
// reply: the status of its error event, or status when it has none.
func Reply(source, target string, req pivot.Request, status int, upstreamStream, agentStream bool, body []byte) (int, []byte) {
	dec, enc, ok := replyCodecs(source, target, req, status, upstreamStream, agentStream)
	if !ok {
		return http.StatusBadGateway, ErrorBody(source, http.StatusBadGateway, "api_error",
			"Cortex cannot convert a "+target+" reply to "+source)
	}
	evs := append(dec.Feed(body), dec.End()...)
	for _, ev := range evs {
		if ev.Kind == pivot.EventError && ev.Status != 0 {
			status = ev.Status
		}
	}
	out := encodeAll(enc, nil, evs)
	return status, append(out, enc.End()...)
}

// encodeAll appends the encoded events to out.
func encodeAll(enc pivot.ReplyEncoder, out []byte, evs []pivot.Event) []byte {
	for _, ev := range evs {
		out = append(out, enc.Encode(ev)...)
	}
	return out
}

// ErrorBody gives an error body in the protocol's shape, or nil for an
// unknown protocol.
func ErrorBody(protocol string, status int, errType, message string) []byte {
	p, ok := protocols[protocol]
	if !ok {
		return nil
	}
	return p.EncodeError(status, errType, message)
}
