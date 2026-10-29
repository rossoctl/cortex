package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
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

// stripForUpstream must still remove the header on a request that is not having its
// WebSocket forwarded — the marker is a record of the strip, not a replacement for it.
// Forwarding Upgrade on an ordinary request would violate RFC 9110's hop-by-hop rule,
// so a change that recorded the value and then left it in place would pass the two
// tests above while breaking the behaviour they describe.
func TestStripForUpstream_StillRemovesTheUpgrade(t *testing.T) {
	r, err := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Upgrade", "h2c")
	r.Header.Set("Connection", "Upgrade")

	stripForUpstream(r, false, false)

	if got := r.Header.Get("Upgrade"); got != "" {
		t.Errorf("Upgrade = %q, want it removed — forwarding it breaks the hop-by-hop rule", got)
	}
	if got := r.Header.Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want it removed", got)
	}
}

// The forwarded case normalizes rather than passes through (#1488): whatever else
// shared the client's Connection and Upgrade lists, the origin sees exactly
// "Connection: Upgrade" and "Upgrade: websocket" — the one protocol this proxy
// forwards, in the one form RFC 9110 §7.8 asks for. The two tests around this one
// hold the other side: a non-websocket token, and the no-upgrade request, strip.
func TestStripForUpstream_ForwardedWebSocketIsNormalized(t *testing.T) {
	r, err := http.NewRequest("GET", "https://ws.example/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Upgrade", "websocket, h2c")
	r.Header.Set("Connection", "keep-alive, Upgrade")

	stripForUpstream(r, false, true)

	if got := r.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("Upgrade = %q, want exactly websocket", got)
	}
	if got := r.Header.Get("Connection"); got != "Upgrade" {
		t.Errorf("Connection = %q, want exactly Upgrade", got)
	}
}

// END TO END, and the only test here that would notice the call sites being deleted:
// the ones above drive pctx.RecordStrippedUpgrade and stripForUpstream themselves, so
// removing serveOutbound's calls leaves them green. This one sends a real request with
// a real Upgrade header through a real proxy and reads both the wire and the recorded
// events.
//
// SINCE #1488 a WebSocket upgrade is FORWARDED: the origin receives Connection:
// Upgrade and Upgrade: websocket, and the request row records no marker — the
// marker is the record of a strip, and nothing was stripped. The origin here
// answers 200 rather than 101, which is an origin declining the upgrade per RFC
// 9110 §7.8; the 200 is relayed the ordinary way, and that ordinary path is what
// leaves the row this test reads. The 101 itself has its own end-to-end test in
// websocketupgrade_test.go.
//
// WITH THE DECLINE OFF, as before: on a Responses path a WebSocket is otherwise
// answered 426 before anything is forwarded (upgradedeclined_test.go), so the
// origin would see nothing and the forwarding checks below would pass without
// testing them at all. That is why it also asserts the origin was reached.
func TestForwardProxy_EndToEnd_ForwardsTheWebSocketUpgrade(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Written by the origin's handler goroutine and read here after the response: a
	// response arriving over a socket is not a happens-before edge Go's memory model
	// recognises, so the three are read under the same lock they are written under.
	var mu sync.Mutex
	var reached bool
	var sawUpgrade, sawConnection string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = true
		sawUpgrade, sawConnection = r.Header.Get("Upgrade"), r.Header.Get("Connection")
		mu.Unlock()
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
	req.Header.Set("Connection", "keep-alive, Upgrade")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	gotReached, gotUpgrade, gotConnection := reached, sawUpgrade, sawConnection
	mu.Unlock()
	if !gotReached {
		t.Fatal("the origin was never reached, so the forwarding below is untested")
	}
	if gotUpgrade != "websocket" {
		t.Errorf("the origin received Upgrade: %q — want websocket, forwarded and normalized", gotUpgrade)
	}
	if gotConnection != "Upgrade" {
		t.Errorf("the origin received Connection: %q — want exactly Upgrade", gotConnection)
	}

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("no events recorded: %+v", v)
	}
	var stripped int
	for _, ev := range v.Events {
		if ev.Phase == pipeline.SessionRequest && ev.UpgradeStripped != "" {
			stripped++
		}
	}
	if stripped != 0 {
		for _, ev := range v.Events {
			t.Logf("  phase=%v upgradeStripped=%q path=%q", ev.Phase, ev.UpgradeStripped, ev.HTTPPath)
		}
		t.Errorf("%d request events carried UpgradeStripped — a forwarded upgrade must not be marked stripped", stripped)
	}
}
