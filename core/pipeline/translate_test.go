package pipeline

import (
	"context"
	"net/http"
	"testing"
)

const (
	srcProto = "anthropic-messages"
	tgtProto = "chat-completions"
)

// redirectThen gives a plugin that redirects to b.example and then runs do.
func redirectThen(do func(*Context) error, err *error) func(*Context) {
	return func(c *Context) {
		if *err = c.Redirect(mustURL("https://b.example")); *err == nil {
			*err = do(c)
		}
	}
}

func TestTranslate_AcceptedAfterARedirect(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		c.Path = "/v1/messages"
		redirectThen(func(c *Context) error { return c.Translate(srcProto, tgtProto, "/v1/chat/completions") }, &err)(c)
	}), true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	src, tgt, path, ok := pctx.TranslateTarget()
	if !ok || src != srcProto || tgt != tgtProto || path != "/v1/chat/completions" {
		t.Errorf("TranslateTarget() = %q, %q, %q, %v", src, tgt, path, ok)
	}
	if pctx.Path != "/v1/messages" {
		t.Errorf("Path = %q, want the client's path: Translate does not change Path", pctx.Path)
	}
	for _, inv := range outbound(pctx) {
		if inv.Reason != "redirected" {
			t.Errorf("invocation %+v: Translate must record nothing", inv)
		}
	}
}

func TestTranslate_SecondCallReplacesTheFirst(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, redirectThen(func(c *Context) error {
		if err := c.Translate(srcProto, tgtProto, "/v1/a"); err != nil {
			return err
		}
		return c.Translate(srcProto, "responses", "/v1/responses")
	}, &err)), true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if _, tgt, path, ok := pctx.TranslateTarget(); !ok || tgt != "responses" || path != "/v1/responses" {
		t.Errorf("TranslateTarget() = %q, %q, %v; want the second call", tgt, path, ok)
	}
}

func TestTranslate_RefusedWithoutARedirect(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		err = c.Translate(srcProto, tgtProto, "/v1/chat/completions")
	}), true)
	if err == nil {
		t.Fatal("Translate succeeded with no Redirect in effect")
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		t.Error("TranslateTarget() ok for a request that was not redirected")
	}
}

func TestTranslate_RefusedWithoutTheCapability(t *testing.T) {
	var err error
	runRequest(t, redirector(false, func(c *Context) {
		err = c.Translate(srcProto, tgtProto, "/v1/chat/completions")
	}), true)
	if err == nil {
		t.Fatal("Translate succeeded for a plugin that does not declare WritesDestination")
	}
}

func TestTranslate_RefusedInTheResponsePass(t *testing.T) {
	var err, rerr error
	plug := redirector(true, func(c *Context) { rerr = c.Redirect(mustURL("https://b.example")) })
	plug.onResponse = func(c *Context) { err = c.Translate(srcProto, tgtProto, "/v1/chat/completions") }
	p, pctx := runRequest(t, plug, true)
	if rerr != nil {
		t.Fatalf("Redirect: %v", rerr)
	}
	p.RunResponse(context.Background(), pctx)
	if err == nil {
		t.Fatal("Translate succeeded from OnResponse")
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		t.Error("TranslateTarget() ok after a refusal in OnResponse")
	}
}

func TestTranslate_NothingUnderObserve(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		_ = c.Redirect(mustURL("https://b.example"))
		err = c.Translate(srcProto, tgtProto, "/v1/chat/completions")
	}), true, WithPolicies(ErrorPolicyObserve))
	if err == nil {
		t.Error("Translate succeeded under observe, where the Redirect moved nothing")
	}
	if _, _, _, ok := pctx.TranslateTarget(); ok {
		t.Error("TranslateTarget() ok under observe")
	}
}

func TestTranslate_RefusesBadArguments(t *testing.T) {
	for _, tc := range []struct{ src, tgt, path string }{
		{srcProto, tgtProto, "v1/x"},
		{srcProto, tgtProto, "/a?b"},
		{srcProto, tgtProto, "/a#b"},
		{srcProto, tgtProto, "/a/../b"},
		{srcProto, tgtProto, ""},
		{srcProto, srcProto, "/v1/messages"},
		{"", tgtProto, "/v1/chat/completions"},
		{srcProto, "", "/v1/chat/completions"},
	} {
		var err error
		_, pctx := runRequest(t, redirector(true, redirectThen(func(c *Context) error {
			return c.Translate(tc.src, tc.tgt, tc.path)
		}, &err)), true)
		if err == nil {
			t.Errorf("Translate(%q, %q, %q) succeeded", tc.src, tc.tgt, tc.path)
		}
		if _, _, _, ok := pctx.TranslateTarget(); ok {
			t.Errorf("Translate(%q, %q, %q) left a target to apply", tc.src, tc.tgt, tc.path)
		}
	}
}

func TestSetUpstreamHeaders_KeepsAClone(t *testing.T) {
	var err error
	h := http.Header{"User-Agent": {"bob-shell"}}
	_, pctx := runRequest(t, redirector(true, redirectThen(func(c *Context) error {
		return c.SetUpstreamHeaders(h)
	}, &err)), true)
	if err != nil {
		t.Fatalf("SetUpstreamHeaders: %v", err)
	}
	h.Set("User-Agent", "changed")
	h.Set("X-Later", "1")
	got := pctx.UpstreamHeaders()
	if got.Get("User-Agent") != "bob-shell" || got.Get("X-Later") != "" {
		t.Errorf("UpstreamHeaders() = %v; a later change to the caller's map must not show", got)
	}
	if len(outbound(pctx)) != 1 {
		t.Errorf("invocations = %+v, want only modify/redirected", outbound(pctx))
	}
}

func TestSetUpstreamHeaders_Refusals(t *testing.T) {
	h := http.Header{"User-Agent": {"bob-shell"}}
	cases := map[string]func(t *testing.T) (error, *Context){
		"no redirect": func(t *testing.T) (error, *Context) {
			var err error
			_, pctx := runRequest(t, redirector(true, func(c *Context) { err = c.SetUpstreamHeaders(h) }), true)
			return err, pctx
		},
		"no capability": func(t *testing.T) (error, *Context) {
			var err error
			_, pctx := runRequest(t, redirector(false, func(c *Context) { err = c.SetUpstreamHeaders(h) }), true)
			return err, pctx
		},
		"observe": func(t *testing.T) (error, *Context) {
			var err error
			_, pctx := runRequest(t, redirector(true, func(c *Context) {
				_ = c.Redirect(mustURL("https://b.example"))
				err = c.SetUpstreamHeaders(h)
			}), true, WithPolicies(ErrorPolicyObserve))
			return err, pctx
		},
		"response pass": func(t *testing.T) (error, *Context) {
			var err error
			plug := redirector(true, func(c *Context) { _ = c.Redirect(mustURL("https://b.example")) })
			plug.onResponse = func(c *Context) { err = c.SetUpstreamHeaders(h) }
			p, pctx := runRequest(t, plug, true)
			p.RunResponse(context.Background(), pctx)
			return err, pctx
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err, pctx := run(t)
			if err == nil {
				t.Error("SetUpstreamHeaders succeeded")
			}
			if pctx.UpstreamHeaders() != nil {
				t.Errorf("UpstreamHeaders() = %v, want nil", pctx.UpstreamHeaders())
			}
		})
	}
}
