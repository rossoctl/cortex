package forwardproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// hopTunnelServer is a forward proxy with a Hop to gatewayAddr, for requests that a
// bridged tunnel decrypted. The tests call bridgedHandler directly: the TLS layer is
// not part of the join.
func hopTunnelServer(t *testing.T, gatewayAddr string, notReady bool) (*Server, *session.Store) {
	t.Helper()
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	hop := NewHop(gatewayAddr, hopTestToken)
	hop.ready.Store(!notReady)
	return &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           &http.Client{Transport: &http.Transport{Proxy: nil}},
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Hop:              hop,
	}, store
}

// serveBridged sends one decrypted request on tl, as bridgedHandler gets it.
func serveBridged(s *Server, tl *tunnelLog, path string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "example.com"
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	rec := httptest.NewRecorder()
	s.bridgedHandler("example.com:443", tl).ServeHTTP(rec, req)
	return rec.Code
}

// gatewayRow is the request row that ingestion gets from the gateway for path.
func gatewayRow(path string) pipeline.SessionEvent { return decryptedRequest(path) }

// waitEvents polls until the store holds n events, or fails after 2 s.
func waitEvents(t *testing.T, store *session.Store, n int) []pipeline.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := allEvents(t, store)
		if len(got) == n || time.Now().After(deadline) {
			if len(got) != n {
				t.Fatalf("store holds %d events, want %d: %+v", len(got), n, got)
			}
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHopJoin_TunnelClosedBeforeRowGetsOnePair(t *testing.T) {
	gw := newFakeGateway(t, nil)
	s, store := hopTunnelServer(t, gatewayAddr(gw), false)
	tl := deferredTunnel(s)
	if st := serveBridged(s, tl, "/v1/x"); st != http.StatusOK {
		t.Fatalf("status = %d, want 200", st)
	}
	tl.finish() // the client closed the tunnel
	if got := allEvents(t, store); len(got) != 0 {
		t.Fatalf("store holds %d rows before the gateway row joined, want none: %+v", len(got), got)
	}
	id := gw.only(t).header.Get(headerRequestID)
	if !s.Hop.AppendRequest(id, "sess-1", gatewayRow("/v1/x"), nil) {
		t.Fatal("AppendRequest = false, want the row paired with the tunnel open")
	}
	_, next := openThen(t, store, "sess-1")
	if next.HTTPPath != "/v1/x" {
		t.Errorf("open precedes %q, want /v1/x", next.HTTPPath)
	}
	if got := allEvents(t, store); len(got) != 2 {
		t.Errorf("store holds %d rows, want the open and the request", len(got))
	}
	if _, closes := tunnelRows(store, "sess-1"); len(closes) != 0 {
		t.Errorf("a joined tunnel recorded %d close row(s)", len(closes))
	}
}

func TestHopJoin_ExpiredJoinSettlesAsToday(t *testing.T) {
	gw := newFakeGateway(t, nil)
	s, store := hopTunnelServer(t, gatewayAddr(gw), false)
	s.Hop.ttl = 50 * time.Millisecond
	tl := deferredTunnel(s)
	serveBridged(s, tl, "/v1/x")
	tl.finish()
	got := waitEvents(t, store, 2)
	if !got[0].Tunnel || got[0].Phase != pipeline.SessionRequest {
		t.Errorf("first row = %+v, want the tunnel open", got[0])
	}
	if !got[1].Tunnel || got[1].Phase != pipeline.SessionResponse {
		t.Errorf("second row = %+v, want the tunnel close", got[1])
	}
}

func TestHopJoin_ReleaseWritesNothing(t *testing.T) {
	gw := newFakeGateway(t, nil)
	s, store := hopTunnelServer(t, gatewayAddr(gw), false)
	s.Hop.ttl = 50 * time.Millisecond
	tl := deferredTunnel(s)
	serveBridged(s, tl, "/v1/x")
	tl.finish()
	s.Hop.Release(gw.only(t).header.Get(headerRequestID))
	time.Sleep(2 * s.Hop.ttl)
	if got := allEvents(t, store); len(got) != 0 {
		t.Errorf("store holds %d rows after Release, want none: %+v", len(got), got)
	}
}

func TestHopJoin_SecondRequestAppendsItself(t *testing.T) {
	gw := newFakeGateway(t, nil)
	s, _ := hopTunnelServer(t, gatewayAddr(gw), false)
	tl := deferredTunnel(s)
	serveBridged(s, tl, "/one")
	serveBridged(s, tl, "/two")
	tl.finish()
	got := gw.requests()
	if len(got) != 2 {
		t.Fatalf("gateway got %d requests, want 2", len(got))
	}
	if !s.Hop.AppendRequest(got[0].header.Get(headerRequestID), "sess-1", gatewayRow("/one"), nil) {
		t.Error("first AppendRequest = false, want true")
	}
	if s.Hop.AppendRequest(got[1].header.Get(headerRequestID), "sess-1", gatewayRow("/two"), nil) {
		t.Error("second AppendRequest = true, want false: the open is already recorded")
	}
}

func TestHopJoin_PlainRequestHasNoPairJoin(t *testing.T) {
	gw := newFakeGateway(t, nil)
	var srv *Server
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, func(s *Server) { srv = s })
	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	doVia(t, hopClient(proxy), req)
	if srv.Hop.AppendRequest(gw.only(t).header.Get(headerRequestID), "sess-1", gatewayRow("/p"), nil) {
		t.Error("AppendRequest = true for a plain request, want false")
	}
}

// The idle cutoff is a 5-minute constant, so this test uses the other proxy_error cut
// of the stream relay, the frame cap; a streaming plugin puts the stream on the frame
// reader. Branch point 5 keys on the kind, not the cause.
func TestHopJoin_IdleCutIsTaken(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: " + strings.Repeat("a", maxBodySize+1)))
	})
	var srv *Server
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, func(s *Server) { srv = s }, newStreamingProbe(false))
	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/stream", nil)
	if resp, err := hopClient(proxy).Do(req); err == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	id := gw.only(t).header.Get(headerRequestID)
	deadline := time.Now().Add(2 * time.Second)
	cut := srv.Hop.TakeCut(id)
	for cut == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		cut = srv.Hop.TakeCut(id)
	}
	if cut == nil || cut.Kind != "proxy_error" {
		t.Fatalf("TakeCut = %+v, want kind proxy_error", cut)
	}
	if again := srv.Hop.TakeCut(id); again != nil {
		t.Errorf("second TakeCut = %+v, want nil", again)
	}
}

func TestHopJoin_GatewayUnavailablePairsAndReleases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		notReady bool
	}{{"not ready", true}, {"dial failure", false}} {
		t.Run(tc.name, func(t *testing.T) {
			addr := closedAddr(t)
			s, store := hopTunnelServer(t, addr, tc.notReady)
			s.Hop.ttl = 50 * time.Millisecond
			tl := deferredTunnel(s)
			if st := serveBridged(s, tl, "/v1/x"); st != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", st)
			}
			tl.finish()
			time.Sleep(2 * s.Hop.ttl)
			_, next := openThen(t, store, "sess-1")
			if next.HTTPPath != "/v1/x" {
				t.Errorf("open precedes %q, want the request row", next.HTTPPath)
			}
			got := allEvents(t, store)
			if len(got) != 3 {
				t.Fatalf("store holds %d rows, want open, request and 503: %+v", len(got), got)
			}
			if r := got[2]; r.StatusCode != http.StatusServiceUnavailable || r.Error == nil || r.Error.Kind != pipeline.ErrorKindGatewayUnavailable {
				t.Errorf("third row = %+v, want the 503 gateway_unavailable row", r)
			}
			if _, closes := tunnelRows(store, "sess-1"); len(closes) != 0 {
				t.Errorf("recorded %d close row(s), want none", len(closes))
			}
			if tl.awaiting != 0 {
				t.Errorf("awaiting = %d after the 503, want 0", tl.awaiting)
			}
		})
	}
}
