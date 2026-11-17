package forwardproxy

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/bodycount"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/translate"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

// translation is a request that the listener converts to the protocol of the
// server, as a plugin asked with pctx.Translate. It goes through the handler as a
// local, not through pctx, so no plugin can change it.
type translation struct {
	source, target, path string
	conv                 translate.Converted
	dec                  pivot.ReplyDecoding // the target's reply decoder
	enc                  pivot.ReplyEncoding // the source's reply encoder
}

// startTranslation converts pctx.Body when a plugin asked for it. It is called after
// every request plugin has run (rule 8). It gives nil, nil when no plugin asked. On a
// refusal it gives the Reject action, with the error body in the agent's protocol;
// nothing must then go upstream. On success it records modify/translated, with the
// fields that the target could not carry (decision 17).
func startTranslation(pctx *pipeline.Context) (*translation, *pipeline.Action) {
	src, tgt, path, ok := pctx.TranslateTarget()
	if !ok {
		return nil, nil
	}
	t := &translation{source: src, target: tgt, path: path}
	conv, err := t.convert(pctx)
	if err != nil {
		status, errType := http.StatusBadGateway, "api_error"
		var re *pivot.RequestError
		if errors.As(err, &re) {
			status, errType = re.Status, re.Type
		}
		msg := err.Error()
		if re != nil {
			msg = re.Message
		}
		pctx.Record(pipeline.Invocation{
			Plugin:  "forward-proxy",
			Action:  pipeline.ActionDeny,
			Phase:   pipeline.InvocationPhaseRequest,
			Reason:  "translation_refused",
			Details: map[string]string{"from": src, "to": tgt, "message": msg},
		})
		return nil, &pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{
			Code:     "inference.translation_refused",
			Reason:   msg,
			Status:   status,
			Body:     translate.ErrorBody(src, status, errType, msg),
			BodyType: "application/json",
		}}
	}
	details := map[string]string{"from": src, "to": tgt, "path": path}
	if len(conv.Dropped) > 0 {
		details["dropped"] = strings.Join(conv.Dropped, ",")
	}
	pctx.Record(pipeline.Invocation{
		Plugin:  "forward-proxy",
		Action:  pipeline.ActionModify,
		Phase:   pipeline.InvocationPhaseRequest,
		Reason:  "translated",
		Details: details,
	})
	t.conv = conv
	return t, nil
}

// convert converts pctx.Body. It removes the source protocol's own headers from
// pctx.Headers (decision 18), so the header sync removes them from the wire. It
// refuses a pair whose reply Cortex cannot convert back, before anything is sent.
func (t *translation) convert(pctx *pipeline.Context) (translate.Converted, error) {
	if pctx.Body == nil {
		return translate.Converted{}, errors.New("the request body was not buffered, so Cortex cannot translate it")
	}
	tp, _ := translate.Lookup(t.target)
	sp, _ := translate.Lookup(t.source)
	var okDec, okEnc bool
	t.dec, okDec = tp.(pivot.ReplyDecoding)
	t.enc, okEnc = sp.(pivot.ReplyEncoding)
	if !okDec || !okEnc {
		return translate.Converted{}, errors.New("Cortex cannot convert a " + t.target + " reply to " + t.source)
	}
	return translate.Request(t.source, t.target, pctx.Body, pctx.Headers)
}

// applyUpstreamHeaders sets the headers that a plugin gave with
// pctx.SetUpstreamHeaders on the upstream request. Each one replaces a header of
// the same name (rule C3). It runs after the sync of pctx.Headers.
func applyUpstreamHeaders(r *http.Request, h http.Header) {
	for k, v := range h {
		r.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
	}
}

// apply puts the converted body and the target path on the upstream request, with
// no query string. pctx.Path does not change: the request row keeps the client's path.
func (t *translation) apply(r *http.Request) {
	httpx.SetRequestBody(r, t.conv.Body)
	r.URL.Path = t.path
	r.URL.RawPath, r.URL.RawQuery, r.URL.ForceQuery = "", "", false
}

// agentStream tells if the agent gets a stream for an upstream answer with this
// status: only a 2xx answer to a stream request (decision 4).
func (t *translation) agentStream(status int) bool {
	return t.conv.Stream && status/100 == 2
}

// wrapReply makes resp carry the reply in the agent's protocol, converted as it is
// read (rule 10). Every later arm then reads the converted bytes. When rc is not nil,
// it keeps the server's reply before the conversion in rc.Upstream (rule 8a).
func (t *translation) wrapReply(resp *http.Response, rc *pipeline.ResponseCapture) {
	if rc != nil {
		// The count and the end are the converted body's, which the caller's wrapper keeps.
		var unused int64
		var ended bool
		resp.Body = bodycount.WrapCapture(resp.Body, &unused, &rc.Upstream, &rc.UpstreamTruncated, &ended, pipeline.MaxResponseCapture)
	}
	stream := t.agentStream(resp.StatusCode)
	dec := t.dec.NewReplyDecoder(resp.StatusCode, httpx.IsEventStream(resp.Header.Get("Content-Type")))
	enc := t.enc.NewReplyEncoder(stream)
	resp.Body = translate.NewReplyReader(resp.Body, dec, enc)
	if stream {
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		resp.Header.Set("Content-Type", "application/json")
	}
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	resp.ContentLength = -1
}

// replyBody converts a whole upstream body, for the row of a refused attempt
// (decision 19).
func (t *translation) replyBody(resp *http.Response, body []byte) []byte {
	return translate.Reply(t.source, t.target, resp.StatusCode,
		httpx.IsEventStream(resp.Header.Get("Content-Type")), t.agentStream(resp.StatusCode), body)
}
