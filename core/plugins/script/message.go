package script

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/redact"
	"golang.org/x/net/http/httpguts"
)

// secretHeaders never reach a script. A script is the user's own code, but its
// output lands in logs and on the timeline, and a credential has no business in
// either.
var secretHeaders = redact.SecretHeaders

// requestMessage is what a script's request hook receives. The parsed extensions
// keep their own JSON tags — the shape GET /v1/sessions/{id} serves — so a script
// tested by replaying recorded events sees exactly what live traffic gives it.
//
// The body and the inference extension go in more, not in the message: the
// script fetches them when a hook reads them. On a long conversation each one is
// about the size of the request, and a script that only reads the host or a
// header should not cost two copies of it per request.
func requestMessage(pctx *pipeline.Context) (msg, more map[string]any) {
	req := map[string]any{
		"direction": pctx.Direction.String(),
		"method":    pctx.Method,
		"host":      pctx.Host,
		"path":      pctx.Path,
		"headers":   flatten(pctx.Headers),
	}
	more = map[string]any{}
	if pctx.Body != nil {
		more["body"] = lazyBody(pctx.Body)
	}
	if e := pctx.Extensions.Inference; e != nil {
		more["inference"] = e
	}
	if e := pctx.Extensions.MCP; e != nil {
		req["mcp"] = e
	}
	if e := pctx.Extensions.A2A; e != nil {
		req["a2a"] = e
	}
	return map[string]any{
		"hook":    "request",
		"session": sessionOf(pctx),
		"request": req,
	}, more
}

// lazyBody encodes as the object putBody builds, so the copy to a string is made
// only when a script fetches the body.
type lazyBody []byte

func (b lazyBody) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	putBody(m, b)
	return json.Marshal(m)
}

func sessionOf(pctx *pipeline.Context) map[string]any {
	s := map[string]any{"request_id": pctx.RequestID()}
	// Key on the view, not the id: a view with an empty id still sends "id": "".
	if pctx.Session != nil {
		s["id"] = pctx.SessionID
	}
	return s
}

func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		if secretHeaders[lk] {
			continue
		}
		out[lk] = strings.Join(vs, ",")
	}
	return out
}

// putBody carries body as text when it is UTF-8, which every JSON API body is, and
// as base64 otherwise, so a binary body survives the trip unchanged.
func putBody(m map[string]any, body []byte) {
	if body == nil {
		return
	}
	if utf8.Valid(body) {
		m["body"] = string(body)
	} else {
		m["body_base64"] = base64.StdEncoding.EncodeToString(body)
	}
}

// reply is what a script answers. Every field is optional; an empty reply means
// "continue, unchanged".
type reply struct {
	Deny       *denyReply  `json:"deny"`
	Request    *edits      `json:"request"`
	Response   *edits      `json:"response"`
	Invocation *invocation `json:"invocation"`
	// Error is the SDK reporting an exception in the script's hook.
	Error string `json:"error"`
}

type denyReply struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Status int    `json:"status"`
}

type edits struct {
	Body          *string           `json:"body"`
	BodyBase64    *string           `json:"body_base64"`
	SetHeaders    map[string]string `json:"set_headers"`
	RemoveHeaders []string          `json:"remove_headers"`
}

func (e *edits) changed() bool {
	return e != nil && (e.Body != nil || e.BodyBase64 != nil || len(e.SetHeaders) > 0 || len(e.RemoveHeaders) > 0)
}

// body returns the replacement body, or ok=false when the reply carries none.
func (e *edits) body() (b []byte, ok bool, err error) {
	switch {
	case e == nil:
		return nil, false, nil
	case e.Body != nil:
		return []byte(*e.Body), true, nil
	case e.BodyBase64 != nil:
		b, err := base64.StdEncoding.DecodeString(*e.BodyBase64)
		if err != nil {
			return nil, false, fmt.Errorf("body_base64: %w", err)
		}
		return b, true, nil
	}
	return nil, false, nil
}

type invocation struct {
	Action  string            `json:"action"`
	Reason  string            `json:"reason"`
	Details map[string]string `json:"details"`
}

func parseReply(line json.RawMessage) (reply, error) {
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return r, fmt.Errorf("malformed reply: %w", err)
	}
	return r, nil
}

// applyHeaders edits h in place. A script cannot set a header it may not read.
// Every key is checked before any is set, so a refused edit leaves nothing
// half-applied however the map iterates.
func applyHeaders(h http.Header, e *edits) error {
	for k, v := range e.SetHeaders {
		if secretHeaders[strings.ToLower(k)] {
			return fmt.Errorf("may not set %s", k)
		}
		if !httpguts.ValidHeaderFieldName(k) {
			return fmt.Errorf("invalid header name %q", k)
		}
		if !httpguts.ValidHeaderFieldValue(v) {
			return fmt.Errorf("invalid value for header %s", k)
		}
	}
	for k, v := range e.SetHeaders {
		h.Set(k, v)
	}
	for _, k := range e.RemoveHeaders {
		h.Del(k)
	}
	return nil
}
