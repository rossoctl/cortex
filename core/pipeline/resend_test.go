package pipeline

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSetRedirectPath_SendsARedirectedRequestToPath(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		c.Path = "/zen/v1/chat/completions"
		if err = c.Redirect(mustURL("https://b.example")); err == nil {
			err = c.SetRedirectPath("/v1/chat/completions")
		}
	}), true)

	if err != nil {
		t.Fatalf("SetRedirectPath: %v", err)
	}
	if path, ok := pctx.RedirectPath(); !ok || path != "/v1/chat/completions" {
		t.Errorf("RedirectPath() = %q, %v; want /v1/chat/completions, true", path, ok)
	}
	if pctx.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want the new path, so the row names where the bytes went", pctx.Path)
	}
	invs := outbound(pctx)
	last := invs[len(invs)-1]
	if last.Reason != "path_rewritten" || last.Details["from"] != "/zen/v1/chat/completions" || last.Details["to"] != "/v1/chat/completions" {
		t.Errorf("last invocation = %+v, want modify/path_rewritten from the client's path", last)
	}
}

func TestSetRedirectPath_RefusedWithoutARedirect(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		err = c.SetRedirectPath("/v1/messages")
	}), true)
	if err == nil {
		t.Fatal("SetRedirectPath succeeded with no Redirect in effect")
	}
	if _, ok := pctx.RedirectPath(); ok {
		t.Error("RedirectPath() reports a path for a request that was not redirected")
	}
}

func TestSetRedirectPath_RefusedWithoutTheCapability(t *testing.T) {
	var err error
	runRequest(t, redirector(false, func(c *Context) { err = c.SetRedirectPath("/v1/messages") }), true)
	if err == nil {
		t.Fatal("SetRedirectPath succeeded for a plugin that does not declare WritesDestination")
	}
}

func TestSetRedirectPath_RefusesMalformedPaths(t *testing.T) {
	for _, path := range []string{"", "v1/messages", "/v1/messages?beta=true", "/v1/messages#x", "/v1/../admin"} {
		var err error
		_, pctx := runRequest(t, redirector(true, func(c *Context) {
			if err = c.Redirect(mustURL("https://b.example")); err == nil {
				err = c.SetRedirectPath(path)
			}
		}), true)
		if err == nil {
			t.Errorf("SetRedirectPath(%q) succeeded", path)
		}
		if _, ok := pctx.RedirectPath(); ok {
			t.Errorf("SetRedirectPath(%q) left a path to apply", path)
		}
	}
}

func TestSetRedirectPath_NothingUnderObserve(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		_ = c.Redirect(mustURL("https://b.example"))
		err = c.SetRedirectPath("/v1/messages")
	}), true, WithPolicies(ErrorPolicyObserve))
	if err == nil {
		t.Error("SetRedirectPath succeeded under observe, where the Redirect moved nothing")
	}
	if _, ok := pctx.RedirectPath(); ok {
		t.Error("RedirectPath() reports a path under observe")
	}
}

// resender is a plugin whose Resend runs do.
type resender struct {
	fnPlugin
	do func(c *Context, status int, body []byte) bool
}

func (r *resender) Resend(_ context.Context, pctx *Context, status int, body []byte) bool {
	return r.do(pctx, status, body)
}

// configurable makes resender Configurable, so plugins.Build would wrap it.
type configurable struct{ resender }

func (c *configurable) Configure(json.RawMessage) error { return nil }

func newResender(writes bool, do func(c *Context, status int, body []byte) bool) *resender {
	return &resender{
		fnPlugin: fnPlugin{name: "router", caps: PluginCapabilities{WritesRequestBody: writes, Description: "test"}},
		do:       do,
	}
}

func TestResend_AsksAResenderAndAppliesItsRewrite(t *testing.T) {
	var gotStatus int
	var gotBody string
	var err error
	r := newResender(true, func(c *Context, status int, body []byte) bool {
		gotStatus, gotBody = status, string(body)
		err = c.SetRequestModel("glm-5-3")
		return true
	})
	p, err2 := New([]Plugin{r})
	if err2 != nil {
		t.Fatal(err2)
	}
	if !p.HasResenders() {
		t.Fatal("HasResenders() = false with a Resender in the chain")
	}
	pctx := &Context{Direction: Outbound, Body: []byte(`{"model":"exo-free","messages":[]}`)}
	if !p.Resend(context.Background(), pctx, 403, []byte(`{"error":{}}`)) {
		t.Fatal("Resend() = false, want the Resender's true")
	}
	if err != nil {
		t.Fatalf("SetRequestModel inside Resend: %v", err)
	}
	if gotStatus != 403 || gotBody != `{"error":{}}` {
		t.Errorf("Resender saw %d %q, want the refused answer", gotStatus, gotBody)
	}
	if m, _ := pctx.RequestModel(); m != "glm-5-3" {
		t.Errorf("model after Resend = %q, want glm-5-3", m)
	}
}

func TestResend_RefusesARedirect(t *testing.T) {
	var err error
	r := newResender(true, func(c *Context, _ int, _ []byte) bool {
		c.MarkRedirectable()
		err = c.Redirect(mustURL("https://b.example"))
		return false
	})
	p, _ := New([]Plugin{r})
	p.Resend(context.Background(), &Context{Direction: Outbound}, 403, nil)
	if err == nil {
		t.Error("Redirect succeeded inside Resend; the destination is fixed by the first send")
	}
}

func TestResend_AsksThroughTheConfiguredWrapper(t *testing.T) {
	asked := false
	inner := &configurable{resender: *newResender(false, func(*Context, int, []byte) bool { asked = true; return false })}
	p, err := New([]Plugin{WrapConfigured(inner, json.RawMessage(`{}`))})
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasResenders() {
		t.Fatal("HasResenders() = false for a wrapped Resender")
	}
	p.Resend(context.Background(), &Context{Direction: Outbound}, 403, nil)
	if !asked {
		t.Error("a wrapped Resender was not asked")
	}
}

func TestResend_AConfiguredPluginIsNotAResenderByBeingWrapped(t *testing.T) {
	p, err := New([]Plugin{WrapConfigured(&fnPlugin{name: "plain", caps: PluginCapabilities{Description: "test"}}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if p.HasResenders() {
		t.Error("HasResenders() = true for a plugin that implements no Resend")
	}
}

func TestResend_SkipsAResenderUnderObserve(t *testing.T) {
	asked := false
	r := newResender(false, func(*Context, int, []byte) bool { asked = true; return true })
	p, err := New([]Plugin{r}, WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	if p.HasResenders() {
		t.Error("HasResenders() = true for a Resender under observe")
	}
	if p.Resend(context.Background(), &Context{Direction: Outbound}, 403, nil) || asked {
		t.Error("a Resender under observe was asked, or its answer used")
	}
}

func TestRenewRequestID_GivesANewID(t *testing.T) {
	pctx := &Context{}
	first := pctx.RequestID()
	pctx.RenewRequestID()
	if second := pctx.RequestID(); second == "" || second == first {
		t.Errorf("RequestID after RenewRequestID = %q, want a new id other than %q", second, first)
	}
}
