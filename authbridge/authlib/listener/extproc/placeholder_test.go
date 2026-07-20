package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// mintPlugin rewrites the inbound Authorization header to a minted
// credential. Used to assert handleInbound emits a SetHeaders mutation
// (via replaceTokenResponse) carrying the new value so Envoy rewrites the
// request to the agent.
type mintPlugin struct{}

func (mintPlugin) Name() string { return "mint" }
func (mintPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (mintPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer abph_minted")
	return pipeline.Action{Type: pipeline.Continue}
}
func (mintPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// setHeaderValue extracts the value for the named SetHeaders key from a
// RequestHeaders ProcessingResponse. replaceTokenResponse stores the value
// in RawValue; fall back to Value for robustness. Returns ("", false) when
// the key is absent.
func setHeaderValue(resp *extprocv3.ProcessingResponse, key string) (string, bool) {
	rh := resp.GetRequestHeaders()
	if rh == nil || rh.GetResponse() == nil || rh.GetResponse().GetHeaderMutation() == nil {
		return "", false
	}
	for _, h := range rh.GetResponse().GetHeaderMutation().GetSetHeaders() {
		hv := h.GetHeader()
		if hv == nil || hv.GetKey() != key {
			continue
		}
		if rv := hv.GetRawValue(); len(rv) > 0 {
			return string(rv), true
		}
		return hv.GetValue(), true
	}
	return "", false
}

// TestExtProc_Inbound_AuthorizationMutation: a plugin that rewrites the
// inbound Authorization header must cause handleInbound to emit a SetHeaders
// mutation carrying the new value, so Envoy rewrites the request to the
// agent. Without the inbound emission path this returns a plain allow with
// no auth mutation.
func TestExtProc_Inbound_AuthorizationMutation(t *testing.T) {
	p, err := pipeline.New([]pipeline.Plugin{mintPlugin{}})
	if err != nil {
		t.Fatalf("building pipeline: %v", err)
	}
	srv := &Server{InboundPipeline: pipeline.NewHolder(p)}

	stream := &mockStream{ctx: context.Background()}
	headers := makeHeaders(
		"x-authbridge-direction", "inbound",
		"authorization", "Bearer real-user-token",
		":path", "/api/test",
	)

	resp, _ := srv.handleInbound(stream, headers, nil)

	got, ok := setHeaderValue(resp, "authorization")
	if !ok {
		t.Fatalf("expected SetHeaders mutation for authorization, got %+v", resp)
	}
	if got != "Bearer abph_minted" {
		t.Errorf("authorization = %q, want %q", got, "Bearer abph_minted")
	}

	// The internal direction header must be stripped on the mint path too,
	// otherwise Envoy forwards x-authbridge-direction to the agent.
	if !headerRemoved(resp.GetRequestHeaders().GetResponse(), "x-authbridge-direction") {
		t.Errorf("expected RemoveHeaders to contain x-authbridge-direction, got %+v", resp)
	}
}

// headerRemoved reports whether the CommonResponse's HeaderMutation removes
// the named header. Shared by the header- and body-path mint tests.
func headerRemoved(cr *extprocv3.CommonResponse, key string) bool {
	if cr == nil || cr.GetHeaderMutation() == nil {
		return false
	}
	for _, h := range cr.GetHeaderMutation().GetRemoveHeaders() {
		if h == key {
			return true
		}
	}
	return false
}

// bodyHeaderValue extracts the value for the named SetHeaders key from a
// RequestBody ProcessingResponse. The body path (replaceTokenBodyResponse
// wrapped by withBodyMutation) nests the SetHeaders mutation inside the
// RequestBody's CommonResponse rather than the RequestHeaders response that
// setHeaderValue reads, so it needs its own accessor. replaceTokenBodyResponse
// stores the value in RawValue; fall back to Value for robustness. Returns
// ("", false) when the key is absent.
func bodyHeaderValue(resp *extprocv3.ProcessingResponse, key string) (string, bool) {
	rb := resp.GetRequestBody()
	if rb == nil || rb.GetResponse() == nil || rb.GetResponse().GetHeaderMutation() == nil {
		return "", false
	}
	for _, h := range rb.GetResponse().GetHeaderMutation().GetSetHeaders() {
		hv := h.GetHeader()
		if hv == nil || hv.GetKey() != key {
			continue
		}
		if rv := hv.GetRawValue(); len(rv) > 0 {
			return string(rv), true
		}
		return hv.GetValue(), true
	}
	return "", false
}

// TestExtProc_InboundBody_AuthorizationMutation mirrors
// TestExtProc_Inbound_AuthorizationMutation but exercises the body path
// (handleInboundBody) instead of the header path. A plugin that rewrites the
// inbound Authorization header must cause handleInboundBody to emit a
// SetHeaders mutation carrying the new value — nested in the RequestBody
// response via replaceTokenBodyResponse/withBodyMutation — so Envoy rewrites
// the request to the agent on the body phase too.
func TestExtProc_InboundBody_AuthorizationMutation(t *testing.T) {
	p, err := pipeline.New([]pipeline.Plugin{mintPlugin{}})
	if err != nil {
		t.Fatalf("building pipeline: %v", err)
	}
	srv := &Server{InboundPipeline: pipeline.NewHolder(p)}

	stream := &mockStream{ctx: context.Background()}
	headers := makeHeaders(
		"x-authbridge-direction", "inbound",
		"authorization", "Bearer real-user-token",
		":path", "/api/test",
	)

	resp, _ := srv.handleInboundBody(stream, headers, []byte("{}"))

	got, ok := bodyHeaderValue(resp, "authorization")
	if !ok {
		t.Fatalf("expected SetHeaders mutation for authorization in body response, got %+v", resp)
	}
	if got != "Bearer abph_minted" {
		t.Errorf("authorization = %q, want %q", got, "Bearer abph_minted")
	}

	// The internal direction header must be stripped on the body mint path too.
	if !headerRemoved(resp.GetRequestBody().GetResponse(), "x-authbridge-direction") {
		t.Errorf("expected RemoveHeaders to contain x-authbridge-direction, got %+v", resp)
	}
}
