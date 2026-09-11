package reverseproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestReverseProxyRecordsMethodAndPath is a POPULATION guard for the request
// and response recorders, driven end-to-end through the real proxy rather than
// by calling the recorders directly — so it also proves the response event
// still sees the request's method and path after the context has been threaded
// through modifyResponse.
//
// The reflection guards in pipeline cannot cover this: they prove the field
// reaches the wire and survives MarshalJSON, not that a recording site copied
// pctx.Method / pctx.Path. Deleting both lines from the recorders leaves those
// guards green.
func TestReverseProxyRecordsMethodAndPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{allowOnlyPlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// A query string is present deliberately: pctx.Path must arrive stripped.
	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/work/item?token=shhh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("no events recorded")
	}

	// Both the request and the response event must carry the pair; a response
	// event that lost them would leave half the timeline row blank.
	var sawRequest, sawResponse bool
	for _, ev := range v.Events {
		switch ev.Phase {
		case pipeline.SessionRequest:
			sawRequest = true
		case pipeline.SessionResponse:
			sawResponse = true
		default:
			continue
		}
		if ev.HTTPMethod != http.MethodPost {
			t.Errorf("phase %v: HTTPMethod = %q, want POST", ev.Phase, ev.HTTPMethod)
		}
		if ev.HTTPPath != "/work/item" {
			t.Errorf("phase %v: HTTPPath = %q, want /work/item (query stripped)", ev.Phase, ev.HTTPPath)
		}
	}
	if !sawRequest {
		t.Error("no request-phase event recorded; the request recorder was not exercised")
	}
	if !sawResponse {
		t.Error("no response-phase event recorded; the response recorder was not exercised")
	}
}

// TestReverseProxyRejectCarriesMethodAndPath covers recordInboundReject, the
// denial path, which the end-to-end allow test above cannot reach.
func TestReverseProxyRejectCarriesMethodAndPath(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    http.MethodDelete,
		Host:      "agent.local",
		Path:      "/admin/keys",
		StartedAt: time.Now(),
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "jwt-validation", Phase: pipeline.InvocationPhaseRequest,
					Action: pipeline.ActionDeny, Reason: "bad_token",
				}},
			},
		},
	}
	s.recordInboundReject(pctx, pipeline.Action{Type: pipeline.Reject})

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 denial event, got %v", v)
	}
	ev := v.Events[0]
	if ev.HTTPMethod != http.MethodDelete {
		t.Errorf("HTTPMethod = %q, want DELETE", ev.HTTPMethod)
	}
	if ev.HTTPPath != "/admin/keys" {
		t.Errorf("HTTPPath = %q, want /admin/keys", ev.HTTPPath)
	}
}
