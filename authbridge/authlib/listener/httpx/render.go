// Package httpx contains HTTP-listener helpers shared between the
// forwardproxy and reverseproxy listeners. extproc and extauthz speak
// gRPC and don't use this package.
package httpx

import (
	"net/http"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// WriteRejection renders a pipeline.Reject Action onto an HTTP
// ResponseWriter. Status, headers, and body all come from the action's
// Violation — listeners hand the writer + action over and let the
// pipeline-defined contract drive the response shape.
//
// Safe to call only when action.Violation is non-nil (i.e. the action
// was Type=Reject). The forward/reverse proxy listeners only invoke
// this on the Reject branch of their action switch, matching that
// invariant.
func WriteRejection(w http.ResponseWriter, action pipeline.Action) {
	status, headers, body := action.Violation.Render()
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
