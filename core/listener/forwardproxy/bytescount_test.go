package forwardproxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// bytesOf drives one request through a forward proxy built on the given plugins
// and returns the byte counts the request and response rows reported (#1309).
//
// Returned as the two rows rather than four numbers because the split is the
// contract: a request row counts what was forwarded and nothing else, a response
// row counts what came back and nothing else, and a listener that put both on one
// row would read as a round trip that never happened.
func bytesOf(t *testing.T, plugins []pipeline.Plugin, req *http.Request, upstream http.HandlerFunc) (reqRow, respRow pipeline.SessionEvent) {
	t.Helper()
	p, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// The caller builds the request against a placeholder host so it can be
	// written before the backend exists; retarget it here.
	target := backend.URL + req.URL.Path
	retargeted, err := http.NewRequest(req.Method, target, req.Body)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	retargeted.Header = req.Header
	retargeted.ContentLength = req.ContentLength

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(retargeted)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	// Drained, not just closed: the streamed arms count as the relay reads, so an
	// undrained body would under-report by whatever was still in flight.
	drainAndClose(t, resp)

	// The row is recorded after the relay (io.Copy or the streaming arms' loop),
	// which can finish after the client has drained and returned — wait for it
	// rather than racing the recording.
	waitForResponseEvent(t, store)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a request row and a response row, got %+v", v)
	}
	return v.Events[0], v.Events[1]
}

func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	resp.Body.Close()
}

// TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow is the forward proxy's own
// anchor for the figures the parity suite compares across listeners: with a body
// plugin in the chain, both directions are buffered and both counts are exact.
func TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow(t *testing.T) {
	const reqBody = `{"method":"tools/call","id":1}`
	const respBody = `{"result":"ok"}`

	req, _ := http.NewRequest("POST", "http://placeholder/mcp", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	reqRow, respRow := bytesOf(t, []pipeline.Plugin{&bodyRecorderPlugin{}}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, respBody)
		})

	if reqRow.BytesUp != int64(len(reqBody)) || reqRow.BytesDown != 0 {
		t.Errorf("request row = ↑%d ↓%d, want ↑%d ↓0", reqRow.BytesUp, reqRow.BytesDown, len(reqBody))
	}
	if respRow.BytesDown != int64(len(respBody)) || respRow.BytesUp != 0 {
		t.Errorf("response row = ↑%d ↓%d, want ↑0 ↓%d", respRow.BytesUp, respRow.BytesDown, len(respBody))
	}
}

// TestBytes_PassthroughSSECountsTheFramingToo covers streamPassthrough, the arm an
// event stream takes when no plugin is a StreamingResponder — relayed
// byte-for-byte, never buffered, and reached by no parity fixture, every one of
// which puts a streaming responder in the chain.
//
// The figure is the WIRE length, framing included, which is the whole reason the
// count is taken off the upstream body rather than inside each arm: this arm has
// no frames to add up, and the arm that does sees them with `data: ` and the
// blank-line separators already stripped. See listener/internal/bodycount.
func TestBytes_PassthroughSSECountsTheFramingToo(t *testing.T) {
	const wire = "data: {\"type\":\"message_start\"}\n\ndata: [DONE]\n\n"

	req, _ := http.NewRequest("GET", "http://placeholder/events", nil)

	_, respRow := bytesOf(t, []pipeline.Plugin{}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, wire)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		})

	if respRow.BytesDown != int64(len(wire)) {
		t.Errorf("response row ↓%d, want ↓%d (the framing counts: %q)", respRow.BytesDown, len(wire), wire)
	}
}

// TestBytes_UnbufferedResponseReportsTheFullCount pins the arm this file used
// to excuse: a plain response that no plugin asked to buffer and that is not an
// event stream is relayed by an io.Copy at the bottom of serveOutbound, which
// runs AFTER the row used to be recorded — so this arm reported zero. The row is
// now recorded below that copy, and the count the wrapper finishes during it is
// the figure the row carries.
//
// Still counted at the wire, not guessed from Content-Length, which is -1 on
// anything chunked and deleted outright on every streaming arm. reverseproxy's
// unbuffered relay defers its row to the end of its copy the same way
// (recordOnEOFBody).
func TestBytes_UnbufferedResponseReportsTheFullCount(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://placeholder/plain", nil)

	_, respRow := bytesOf(t, []pipeline.Plugin{}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"never":"buffered"}`)
		})

	if respRow.BytesDown != int64(len(`{"never":"buffered"}`)) {
		t.Errorf("response row ↓%d, want ↓%d — the row is recorded after the copy, so the count is complete when the figure is read",
			respRow.BytesDown, len(`{"never":"buffered"}`))
	}
}
