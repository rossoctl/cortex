package forwardproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// A WebSocket upgrade on a Responses API path is answered 426 by the proxy, because
// the origin's own answer to the stripped request is a 405 that Codex retries for
// about six seconds before falling back to HTTPS. See declineResponsesWebSocket. With
// listener.decline_responses_websockets: false the request is forwarded as before;
// upgradestripped_test.go covers that side.

// declineHarness is a forward proxy in front of an origin that counts what reaches it.
type declineHarness struct {
	store   *session.Store
	client  *http.Client
	origin  *httptest.Server
	reached atomic.Int32
}

// newDeclineHarness builds the proxy with the decline on, as config defaults it.
func newDeclineHarness(t *testing.T, plugins ...pipeline.Plugin) *declineHarness {
	return newDeclineHarnessWith(t, true, plugins...)
}

func newDeclineHarnessWith(t *testing.T, decline bool, plugins ...pipeline.Plugin) *declineHarness {
	t.Helper()
	h := &declineHarness{store: session.New(5*time.Minute, 100, 0)}
	t.Cleanup(h.store.Close)
	h.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.reached.Add(1)
		// What chatgpt.com answers a bare GET on its Responses endpoint.
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"detail":"Method Not Allowed"}`)
	}))
	t.Cleanup(h.origin.Close)

	p, err := plugintesting.BuildPipeline(plugins)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline:           pipeline.NewHolder(p),
		Sessions:                   h.store,
		Client:                     http.DefaultClient,
		DeclineResponsesWebSockets: decline,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	return h
}

func (h *declineHarness) do(t *testing.T, method, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.origin.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// The headers Codex's tungstenite client sends on the handshake, less auth.
func websocketHeaders() http.Header {
	return http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-Websocket-Version": {"13"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Openai-Beta":           {"responses_websockets=2026-02-06"},
	}
}

func TestForwardProxy_ResponsesWebSocketIsDeclinedWith426(t *testing.T) {
	h := newDeclineHarness(t)
	resp, body := h.do(t, http.MethodGet, "/backend-api/codex/responses", websocketHeaders())

	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
	if body != declinedUpgradeBody {
		t.Errorf("body = %q, want %q", body, declinedUpgradeBody)
	}
	// The point of answering locally: the origin's answer is already known.
	if n := h.reached.Load(); n != 0 {
		t.Errorf("the origin was contacted %d time(s); a declined upgrade must not reach it", n)
	}

	v := h.store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a request and a response row, got %+v", v)
	}
	req, res := v.Events[0], v.Events[1]
	if req.Phase != pipeline.SessionRequest || req.UpgradeStripped != "websocket" {
		t.Errorf("request row: phase=%v upgradeStripped=%q, want request/websocket", req.Phase, req.UpgradeStripped)
	}
	if res.Phase != pipeline.SessionResponse || res.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("response row: phase=%v status=%d, want response/426", res.Phase, res.StatusCode)
	}
	if res.Error == nil || res.Error.Kind != "upgrade_declined" {
		t.Errorf("response row error = %+v, want kind upgrade_declined", res.Error)
	}
	if req.RequestID == "" || req.RequestID != res.RequestID {
		t.Errorf("rows do not pair: request %q, response %q", req.RequestID, res.RequestID)
	}
}

// The off switch restores the forwarding exactly: the origin is reached and its own
// 405 is what the client gets — the behaviour before the decline existed.
func TestForwardProxy_DeclineOff_ForwardsTheUpgradeAsBefore(t *testing.T) {
	h := newDeclineHarnessWith(t, false)
	resp, body := h.do(t, http.MethodGet, "/backend-api/codex/responses", websocketHeaders())

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want the origin's 405", resp.StatusCode)
	}
	if body != `{"detail":"Method Not Allowed"}` {
		t.Errorf("body = %q, want the origin's", body)
	}
	if n := h.reached.Load(); n != 1 {
		t.Errorf("origin reached %d time(s), want 1", n)
	}
	v := h.store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no events recorded")
	}
	for _, ev := range v.Events {
		if ev.Error != nil && ev.Error.Kind == "upgrade_declined" {
			t.Errorf("a row says upgrade_declined with the decline off: %+v", ev)
		}
	}
}

// The public API's path is declined too: the predicate is the Responses suffix
// inference-parser reads, not Codex's one URL.
func TestForwardProxy_PublicResponsesPathIsDeclinedToo(t *testing.T) {
	h := newDeclineHarness(t)
	resp, _ := h.do(t, http.MethodGet, "/v1/responses", websocketHeaders())
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
}

// Everything outside the shape keeps today's behaviour: the strip, and the origin's
// own answer. 426 means "use HTTPS" only to Responses clients, so a WebSocket on any
// other path, or a request that is not an upgrade, must still reach the origin.
func TestForwardProxy_OnlyResponsesWebSocketsAreDeclined(t *testing.T) {
	upgradeTo := func(proto string) http.Header {
		h := websocketHeaders()
		h.Set("Upgrade", proto)
		return h
	}
	cases := []struct {
		name   string
		method string
		path   string
		header http.Header
	}{
		{"websocket on another path", http.MethodGet, "/socket", websocketHeaders()},
		{"websocket on a path merely containing responses", http.MethodGet, "/responses/stream", websocketHeaders()},
		{"plain GET on the Responses path", http.MethodGet, "/backend-api/codex/responses", nil},
		{"POST on the Responses path", http.MethodPost, "/backend-api/codex/responses", nil},
		{"upgrade to another protocol", http.MethodGet, "/backend-api/codex/responses", upgradeTo("h2c")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeclineHarness(t)
			resp, _ := h.do(t, tc.method, tc.path, tc.header)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want the origin's 405", resp.StatusCode)
			}
			if n := h.reached.Load(); n != 1 {
				t.Errorf("origin reached %d time(s), want 1", n)
			}
		})
	}
}

// The Upgrade token is case-insensitive (RFC 9110 §7.8), and Codex is not the only
// client that might capitalise it.
func TestForwardProxy_WebSocketTokenMatchesCaseInsensitively(t *testing.T) {
	h := newDeclineHarness(t)
	hdr := websocketHeaders()
	hdr.Set("Upgrade", "WebSocket")
	resp, _ := h.do(t, http.MethodGet, "/backend-api/codex/responses", hdr)
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
}

// A plugin's reject still wins: the decline runs after the pipeline, so a policy that
// refuses the request is not overridden by a 426 the client would then route around.
func TestForwardProxy_PluginRejectWinsOverTheDecline(t *testing.T) {
	h := newDeclineHarness(t, denyPlugin{})
	resp, _ := h.do(t, http.MethodGet, "/backend-api/codex/responses", websocketHeaders())
	if resp.StatusCode == http.StatusUpgradeRequired {
		t.Fatal("a denied request was answered 426; the plugin's reject must win")
	}
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin reached %d time(s) on a denied request", n)
	}
}

// The deferred Finisher dispatch must hear an allow with the status the client got.
// Left at zero, OutcomeFromContext reports OutcomeError, and lineage would label a
// clean, deliberate answer as a failure.
func TestForwardProxy_DeclineReportsAnAllowToFinishers(t *testing.T) {
	fin := &outcomeRecorder{}
	h := newDeclineHarness(t, fin)
	h.do(t, http.MethodGet, "/backend-api/codex/responses", websocketHeaders())

	got := fin.seen()
	if got == nil {
		t.Fatal("OnFinish never ran, so the outcome is unasserted")
	}
	if got.FinalAction != pipeline.OutcomeAllow || got.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("outcome = %v/%d, want allow/426", got.FinalAction, got.StatusCode)
	}
}
