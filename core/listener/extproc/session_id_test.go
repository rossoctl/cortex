package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// sessionIDProbe records, for each request, the id of the view it was given and
// pctx.SessionID. A nil view records "-". hosts is how many InferenceHosts rows
// pctx.InferenceHosts() gave, -1 for unknown.
type sessionIDProbe struct {
	view, id []string
	hosts    []int
}

func (*sessionIDProbe) Name() string { return "session-id-probe" }
func (*sessionIDProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (p *sessionIDProbe) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	v := "-"
	if pctx.Session != nil {
		v = pctx.Session.ID
	}
	p.view, p.id = append(p.view, v), append(p.id, pctx.SessionID)
	n := -1
	if h, known := pctx.InferenceHosts(); known {
		n = len(h)
	}
	p.hosts = append(p.hosts, n)
	return pipeline.Action{Type: pipeline.Continue}
}
func (*sessionIDProbe) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Both outbound entry points set pctx.SessionID to the view's id, and leave it
// empty when they give no view. With a view they pass the store's facts: the
// InferenceHosts table is one that the view cannot give.
func TestOutbound_SessionIDIsTheViewID(t *testing.T) {
	headerOnly := []*extprocv3.ProcessingRequest{{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(
			"x-authbridge-direction", "outbound", ":method", "GET", ":path", "/", ":authority", "api.local"),
			EndOfStream: true},
	}}}
	withBody := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(
			"x-authbridge-direction", "outbound", ":method", "POST", ":path", "/", ":authority", "api.local",
			"content-length", "2")}}},
		{Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: &extprocv3.HttpBody{Body: []byte("{}"), EndOfStream: true}}},
	}
	for _, tc := range []struct {
		name     string
		reqs     []*extprocv3.ProcessingRequest
		active   string
		wantView string
	}{
		{"headers, active session", headerOnly, "s1", "s1"},
		{"headers, no session", headerOnly, "", "-"},
		{"body, active session", withBody, "s1", "s1"},
		{"body, no session", withBody, "", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &sessionIDProbe{}
			out, err := pipeline.New([]pipeline.Plugin{probe})
			if err != nil {
				t.Fatal(err)
			}
			empty, _ := pipeline.New(nil)
			store := session.New(0, 100, 100)
			t.Cleanup(store.Close)
			if tc.active != "" {
				// An inference request: a row of the session's InferenceHosts table, which a view cannot give.
				store.Append(tc.active, pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
					Host: "glm.example.com:8443", Client: pipeline.ParseUserAgent("claude-cli/2.1.0 (external, cli)"),
					Inference: &pipeline.InferenceExtension{Model: "m"}})
			}
			srv := &Server{OutboundPipeline: pipeline.NewHolder(out), InboundPipeline: pipeline.NewHolder(empty), Sessions: store}
			_ = srv.Process(&mockStream{ctx: context.Background(), requests: tc.reqs})

			if len(probe.view) != 1 {
				t.Fatalf("probe ran %d times, want 1", len(probe.view))
			}
			wantID := tc.wantView
			if wantID == "-" {
				wantID = ""
			}
			if probe.view[0] != tc.wantView || probe.id[0] != wantID {
				t.Errorf("view %q, SessionID %q; want view %q, SessionID %q", probe.view[0], probe.id[0], tc.wantView, wantID)
			}
			wantHosts := -1 // no session: unknown
			if tc.active != "" {
				wantHosts = 1
			}
			if probe.hosts[0] != wantHosts {
				t.Errorf("InferenceHosts rows = %d, want %d (-1: unknown)", probe.hosts[0], wantHosts)
			}
		})
	}
}
