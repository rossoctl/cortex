package reverseproxy

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

// bytesOf drives one request through a reverse proxy built on the given plugins
// and returns the byte counts the request and response rows reported (#1309).
//
// Returned as the two rows rather than four numbers because the split is the
// contract: a request row counts what was forwarded and nothing else, a response
// row counts what came back and nothing else.
func bytesOf(t *testing.T, plugins []pipeline.Plugin, method, path, reqBody string, upstream http.HandlerFunc) (reqRow, respRow pipeline.SessionEvent) {
	t.Helper()
	p, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// A nil reader, not an empty one: an empty strings.Reader is still a body as
	// far as net/http is concerned, and the body-less case is one this is here to
	// measure.
	req, _ := http.NewRequest(method, proxy.URL+path, nil)
	if reqBody != "" {
		req, _ = http.NewRequest(method, proxy.URL+path, strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	// Drained, not just closed: the streamed arms count as the relay reads, so an
	// undrained body would under-report by whatever was still in flight.
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	resp.Body.Close()

	// Waited rather than read: the unbuffered relay defers its response row to
	// the end of ReverseProxy's copy (recordOnEOFBody), which can finish after
	// the body was drained here.
	waitForResponseEvent(t, store, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a request row and a response row, got %+v", v)
	}
	return v.Events[0], v.Events[1]
}

// TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow is the reverse proxy's own
// anchor for the figures the parity suite compares across listeners: with a body
// plugin in the chain, both directions are buffered and both counts are exact.
func TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow(t *testing.T) {
	const reqBody = `{"jsonrpc":"2.0","method":"message/send"}`
	const respBody = `{"jsonrpc":"2.0","result":{}}`

	reqRow, respRow := bytesOf(t, []pipeline.Plugin{&bodyRecorderPlugin{}}, "POST", "/a2a", reqBody,
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

// TestBytes_UnbufferedResponseReportsTheFullCount pins the arm this file used
// to excuse: with no plugin asking for the response body, httputil.ReverseProxy
// copies it straight to the client — AFTER modifyResponse returns. The row used
// to be appended inside modifyResponse, before the copy, so this arm reported
// zero; it is now deferred to the end of the copy (recordOnEOFBody), and the
// count the wrapper finishes during that copy is the figure the row carries.
//
// Still counted at the wire, not guessed from Content-Length, which is -1 on
// anything chunked and deleted outright on the streaming arm. forwardproxy's
// unbuffered relay records after its copy for the same reason.
func TestBytes_UnbufferedResponseReportsTheFullCount(t *testing.T) {
	_, respRow := bytesOf(t, []pipeline.Plugin{}, "GET", "/plain", "",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"never":"buffered"}`)
		})

	if respRow.BytesDown != int64(len(`{"never":"buffered"}`)) {
		t.Errorf("response row ↓%d, want ↓%d — the row is recorded after the copy, so the count is complete when the figure is read",
			respRow.BytesDown, len(`{"never":"buffered"}`))
	}
}
