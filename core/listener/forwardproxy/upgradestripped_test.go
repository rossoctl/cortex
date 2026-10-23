package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The consumer half of the upgrade marker: pins that a request whose Upgrade header
// the proxy removed reaches the timeline saying so.
//
// Codex is the case this exists for. It asks for
// wss://chatgpt.com/backend-api/codex/responses; stripForUpstream removes Upgrade
// because RFC 9110 makes it hop-by-hop, so the origin sees a bare GET on an endpoint
// that takes a POST or an upgrade and answers 405. Codex falls back to HTTPS and
// works — leaving a user looking at `405 Method Not Allowed` in their agent's output
// with every reason to think Cortex broke it, and an event saying only
// backend_error/405 with nothing joining the two.
func TestRecordOutboundRequest_CarriesStrippedUpgrade(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "GET",
		Host:      "chatgpt.com",
		Path:      "/backend-api/codex/responses",
		StartedAt: time.Now(),
	}
	// What handleRequest does immediately before stripForUpstream.
	pctx.RecordStrippedUpgrade(http.Header{
		"Upgrade":    []string{"websocket"},
		"Connection": []string{"Upgrade"},
	})
	s.recordOutboundRequestEvent(nil, pctx, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	if got := v.Events[0].UpgradeStripped; got != "websocket" {
		t.Errorf("UpgradeStripped = %q, want websocket", got)
	}
}

// The overwhelmingly common row: an ordinary request records nothing, so the field
// stays absent rather than becoming a key on every event. Without this, a mutation
// that recorded unconditionally would pass the test above and still be wrong.
func TestRecordOutboundRequest_OrdinaryRequestRecordsNoUpgrade(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Host:      "api.anthropic.com",
		Path:      "/v1/messages",
		StartedAt: time.Now(),
	}
	pctx.RecordStrippedUpgrade(http.Header{"Content-Type": []string{"application/json"}})
	s.recordOutboundRequestEvent(nil, pctx, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	if got := v.Events[0].UpgradeStripped; got != "" {
		t.Errorf("UpgradeStripped = %q, want empty on a request that asked for no upgrade", got)
	}
}

// stripForUpstream must still remove the header — the marker is a record of the
// strip, not a replacement for it. Forwarding Upgrade would violate RFC 9110's
// hop-by-hop rule, so a change that recorded the value and then left it in place
// would pass the two tests above while breaking the behaviour they describe.
func TestStripForUpstream_StillRemovesTheUpgrade(t *testing.T) {
	r, err := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")

	stripForUpstream(r, false)

	if got := r.Header.Get("Upgrade"); got != "" {
		t.Errorf("Upgrade = %q, want it removed — forwarding it breaks the hop-by-hop rule", got)
	}
	if got := r.Header.Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want it removed", got)
	}
}

// END TO END, and the only test here that would notice the call site being deleted:
// the three above drive pctx.RecordStrippedUpgrade themselves, so removing
// handleRequest's call leaves them green. This one sends a real request with a real
// Upgrade header through a real proxy and reads the recorded event.
//
// It also asserts the OTHER half in the same pass — that the origin received no
// Upgrade — so a change that recorded the marker by keeping the header fails here
// rather than looking like an improvement.
//
// WITH THE DECLINE OFF. On a Responses path a WebSocket is otherwise answered 426
// before anything is forwarded (upgradedeclined_test.go), so the origin would see
// nothing and the "received no Upgrade" check below would pass without testing the
// strip at all. That is why it also asserts the origin was reached.
func TestForwardProxy_EndToEnd_RecordsAndStripsTheUpgrade(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	var reached bool
	var sawUpgrade, sawConnection string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		sawUpgrade, sawConnection = r.Header.Get("Upgrade"), r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline:           pipeline.NewHolder(p),
		Sessions:                   store,
		Client:                     http.DefaultClient,
		DeclineResponsesWebSockets: false,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	req, err := http.NewRequest("GET", backend.URL+"/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !reached {
		t.Fatal("the origin was never reached, so the strip below is untested")
	}
	if sawUpgrade != "" {
		t.Errorf("the origin received Upgrade: %q — it must be stripped", sawUpgrade)
	}
	if sawConnection != "" {
		t.Errorf("the origin received Connection: %q — it must be stripped", sawConnection)
	}

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("no events recorded: %+v", v)
	}
	var found bool
	for _, ev := range v.Events {
		if ev.Phase == pipeline.SessionRequest && ev.UpgradeStripped == "websocket" {
			found = true
		}
	}
	if !found {
		for _, ev := range v.Events {
			t.Logf("  phase=%v upgradeStripped=%q path=%q", ev.Phase, ev.UpgradeStripped, ev.HTTPPath)
		}
		t.Error("no request event carried UpgradeStripped=websocket — is handleRequest still calling RecordStrippedUpgrade before stripForUpstream?")
	}
}
