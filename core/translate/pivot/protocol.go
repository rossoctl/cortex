package pivot

import (
	"net/http"
	"strconv"
)

// RequestError is a refusal to send to the agent, in the shape of the
// agent's protocol (EncodeError).
type RequestError struct {
	Status        int
	Type, Message string
}

// Error gives the status, the type and the message.
func (e *RequestError) Error() string {
	return strconv.Itoa(e.Status) + " " + e.Type + ": " + e.Message
}

// ErrorType maps an HTTP status to the Anthropic error type of rule 24:
// 400,422 invalid_request_error; 401 authentication_error; 402,403
// permission_error; 404 not_found_error; 429 rate_limit_error; 5xx and
// every other status api_error.
func ErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402, 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	}
	return "api_error"
}

// Protocol is one inference protocol (rule H4). Each direction is an
// optional interface below, and a protocol can leave a direction out.
// Every protocol must supply EncodeError.
type Protocol interface {
	Name() string
	// EncodeError gives an error body in this protocol's shape.
	EncodeError(status int, errType, message string) []byte
}

// RequestDecoder converts a request body of this protocol to the pivot form.
type RequestDecoder interface {
	DecodeRequest(body []byte) (Request, error)
}

// RequestEncoder converts a pivot request to a body of this protocol. It
// removes the source protocol's own headers from h. It does not choose the
// path or the auth header. dropped names each field that it could not send.
type RequestEncoder interface {
	EncodeRequest(req Request, h http.Header) (body []byte, dropped []string, err error)
}

// ReplyDecoding makes a decoder for one upstream reply. status is the
// upstream status, and stream tells if the upstream body is SSE.
type ReplyDecoding interface {
	NewReplyDecoder(status int, stream bool) ReplyDecoder
}

// ReplyEncoding makes an encoder for one reply to the agent. stream tells
// if the agent asked for a stream.
type ReplyEncoding interface {
	NewReplyEncoder(stream bool) ReplyEncoder
}

// ReplyDecoder converts the bytes of an upstream reply to events. It keeps
// state between calls and never holds a whole 2xx stream.
type ReplyDecoder interface {
	Feed(p []byte) []Event
	End() []Event
}

// ReplyEncoder converts events to bytes for the agent. For a stream it
// gives bytes at once. For a not-streamed reply it gives nil until End,
// which gives the one body.
type ReplyEncoder interface {
	Encode(ev Event) []byte
	End() []byte
}
