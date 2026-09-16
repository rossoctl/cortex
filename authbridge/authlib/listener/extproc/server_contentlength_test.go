package extproc

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// Envoy's ext_proc contract for the mode this listener runs (BUFFERED body,
// SEND headers), from api/envoy/extensions/filters/http/ext_proc/v3/
// processing_mode.proto, BodySendMode (v1.37.1):
//
//	In BUFFERED mode with SEND header mode, content length header is
//	allowed but it is external processor's responsibility to set the
//	content length correctly matched to the length of mutated body.
//
// These tests pin our side of that contract: a body-mutation reply carries
// content-length equal to the mutated body. They do not exercise Envoy.
// Envoy's side is pinned by its own integration test at the same tag,
// test/extensions/filters/http/ext_proc/ext_proc_integration_test.cc
// MismatchedContentLengthAndBodyLength: BUFFERED + SEND, the processor
// replaces "Replace this!" with "Hello, World!" and sets content-length to
// the wrong value → the upstream is never reached, downstream gets 500. The
// original body below is the same; the replacement is deliberately longer,
// so a reply that merely echoed the original length would fail too.

func TestWithBodyMutation_SetsContentLength(t *testing.T) {
	pctx := &pipeline.Context{Body: []byte("Replace this!")}
	pctx.SetBody([]byte("Hello, World! (longer)"))

	hm := withBodyMutation(passBodyResponse(), pctx).GetRequestBody().GetResponse().GetHeaderMutation()
	if got, want := mutationHeaderValue(hm, "content-length"), strconv.Itoa(len(pctx.Body)); got != want {
		t.Fatalf("content-length = %q, want %q", got, want)
	}
}

func TestHandleResponseBody_SetsContentLength(t *testing.T) {
	newBody := []byte("Hello, World! (longer)")
	p, err := pipeline.New([]pipeline.Plugin{&responseMutator{newBody}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p)}
	pctx := &pipeline.Context{ResponseHeaders: http.Header{"Content-Encoding": {"gzip"}}}

	resp := srv.handleResponseBody(context.Background(), []byte("Replace this!"), pctx, "", true)
	hm := resp.GetResponseBody().GetResponse().GetHeaderMutation()
	if got, want := mutationHeaderValue(hm, "content-length"), strconv.Itoa(len(newBody)); got != want {
		t.Fatalf("content-length = %q, want %q", got, want)
	}
	if !mutationRemovesHeader(hm, "content-encoding") {
		t.Fatalf("content-encoding not removed: %+v", hm)
	}
}

type responseMutator struct{ newBody []byte }

func (*responseMutator) Name() string { return "response-mutator" }
func (*responseMutator) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesResponseBody: true}
}
func (*responseMutator) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (m *responseMutator) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SetResponseBody(m.newBody)
	return pipeline.Action{Type: pipeline.Continue}
}
