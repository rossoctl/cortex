package contextguru

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/cheapmodel"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/pipeline"
)

// anthropicPctx is a Claude Code style request to host, with a credential.
func anthropicPctx(host string, role pipeline.AgentRole) *pipeline.Context {
	p := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	p.Host = host
	p.Headers = http.Header{"X-Api-Key": {"sk-caller"}}
	p.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5", AgentRole: role}
	return p
}

// An Anthropic request now gets an incoming model; before, every model.source: incoming
// component had none on Claude Code traffic.
func TestAnthropicRequestGetsIncomingModel(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	spec := p.modelsFor(anthropicPctx("gw", pipeline.AgentRoleMain), bschemas.Anthropic, "", "s1")
	if _, ok := spec.Incoming.(cheapmodel.Anthropic); !ok {
		t.Fatalf("Incoming = %#v, want cheapmodel.Anthropic", spec.Incoming)
	}
}

func TestAnthropicRequestWithoutCredentialGetsNoModel(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	pctx.Headers = http.Header{}
	if spec := p.modelsFor(pctx, bschemas.Anthropic, "", "s1"); spec.Incoming != nil {
		t.Fatalf("Incoming = %#v, want nil without a credential", spec.Incoming)
	}
}

// One hold per credential, reused across requests, released once idle past the TTL. The
// summarizer's call is detached, so the hold must outlive the request that made it.
func TestCredHoldsReuseAndExpire(t *testing.T) {
	d := newCortexDispatcher(newPingClient())
	h := newCredHolds(d)
	now := time.Unix(1_700_000_000, 0)
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	a := h.hold(pctx, now)
	if b := h.hold(pctx, now.Add(time.Minute)); b != a {
		t.Fatalf("second hold = %q, want reuse of %q", b, a)
	}
	other := anthropicPctx("gw", pipeline.AgentRoleMain)
	other.Headers.Set("X-Api-Key", "sk-other")
	if c := h.hold(other, now.Add(incomingHoldTTL+2*time.Minute)); c == a {
		t.Fatal("a different credential reused the first hold")
	}
	if _, held := d.held[a]; held {
		t.Fatal("hold idle past the TTL was not released")
	}
	h.releaseAll()
	if len(d.held) != 0 {
		t.Fatalf("releaseAll left %d holds", len(d.held))
	}
}

func TestSentStashReplacesAndDropsOversizedBodies(t *testing.T) {
	s := newSentStash()
	s.put("k", bschemas.Anthropic, []byte("pre"), []byte("sent-1"))
	s.put("k", bschemas.Anthropic, []byte("pre"), []byte("sent-2"))
	if got := string(s.get("k")); got != "sent-2" {
		t.Fatalf("get = %q, want the latest body", got)
	}
	// An oversized body must not leave the older one behind: Ask would answer about a
	// transcript missing everything since.
	s.put("k", bschemas.Anthropic, nil, make([]byte, maxStashBody+1))
	if got := s.get("k"); got != nil {
		t.Fatalf("get after oversized put = %d bytes, want nothing", len(got))
	}
}

// OnFinish stashes a successful main-agent request: the forwarded body for Ask, the received one
// for CoversSpan. A subagent or a failed request stashes nothing.
func TestOnFinishStashesOnlySuccessfulMainRequests(t *testing.T) {
	for name, tc := range map[string]struct {
		role   pipeline.AgentRole
		status int
		want   bool
	}{
		"main 200": {pipeline.AgentRoleMain, 200, true},
		"subagent": {pipeline.AgentRoleSubagent, 200, false},
		"main 500": {pipeline.AgentRoleMain, 500, false},
	} {
		p := configured(t, `{`+offline+`}}`)
		pctx := anthropicPctx("gw", tc.role)
		invokeReq(p, pctx)
		pctx.StatusCode = tc.status
		p.OnFinish(context.Background(), pctx)
		key, _ := p.cgSessions.Load("s1")
		k, _ := key.(string)
		if got := p.stash.get(k) != nil; got != tc.want {
			t.Errorf("%s: stashed=%v, want %v", name, got, tc.want)
		}
	}
}

// The asker appends its question to the stashed forwarded body and sends it on the caller's own
// credential; with nothing stashed it reports ErrNoPrefix, which the summarizer handles.
func TestPrefixAskerAppendsToStashedBody(t *testing.T) {
	var gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5","content":[{"type":"text","text":"summary"}],`+
			`"usage":{"input_tokens":1,"cache_read_input_tokens":9,"output_tokens":2}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
	pctx.Scheme = "http"
	cli, ok := p.anthropicIncoming(pctx, nil)
	if !ok {
		t.Fatal("no incoming model")
	}
	a := prefixAsker{stash: p.stash, cli: cli}
	if _, _, err := a.Ask(context.Background(), "k", "summarize"); !errors.Is(err, cgcomponents.ErrNoPrefix) {
		t.Fatalf("Ask with nothing stashed: err = %v, want ErrNoPrefix", err)
	}
	p.stash.put("k", bschemas.Anthropic, []byte(`{}`),
		[]byte(`{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"the transcript"}]}`))
	reply, _, err := a.Ask(context.Background(), "k", "summarize")
	if err != nil || reply != "summary" {
		t.Fatalf("Ask = %q, %v; want summary", reply, err)
	}
	if gotKey != "sk-caller" || !strings.Contains(gotBody, "the transcript") || !strings.Contains(gotBody, "summarize") {
		t.Fatalf("upstream got key %q and body %q; want the caller's key and the stashed body plus the ask", gotKey, gotBody)
	}
}
