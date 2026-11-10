package forwardproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/egressguard"
)

const egressTestToken = "tok-123"

// egressRig is an origin and an egress listener in front of it.
type egressRig struct {
	origin *httptest.Server
	egress *httptest.Server
	hits   atomic.Int32
}

func newEgressRig(t *testing.T, token string, origin http.HandlerFunc) *egressRig {
	t.Helper()
	rig := &egressRig{}
	rig.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		origin(w, r)
	}))
	t.Cleanup(rig.origin.Close)
	rig.egress = httptest.NewServer(NewEgressHandler(token, http.DefaultTransport.(*http.Transport).Clone()))
	t.Cleanup(rig.egress.Close)
	return rig
}

func (rig *egressRig) dest() string { return rig.origin.URL } // http://127.0.0.1:port

func egressDo(t *testing.T, rig *egressRig, path, token, dest string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rig.egress.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	if token != "" {
		req.Header.Set(headerToken, token)
	}
	if dest != "" {
		req.Header.Set(headerDest, dest)
	}
	resp, err := (&http.Transport{Proxy: nil, DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func okOrigin(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestEgress_TokenRequired(t *testing.T) {
	for _, tc := range []struct{ name, configured, given string }{
		{"no token", egressTestToken, ""},
		{"wrong token", egressTestToken, "nope"},
		{"empty configured token", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEgressRig(t, tc.configured, okOrigin)
			resp := egressDo(t, rig, "/", tc.given, rig.dest(), nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if n := rig.hits.Load(); n != 0 {
				t.Fatalf("origin saw %d requests, want 0", n)
			}
		})
	}
}

func TestEgress_ProbeAnswers204(t *testing.T) {
	rig := newEgressRig(t, egressTestToken, okOrigin)
	resp := egressDo(t, rig, "/", egressTestToken, probeDest, http.Header{headerProbe: {"1"}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if n := rig.hits.Load(); n != 0 {
		t.Fatalf("origin saw %d requests, want 0", n)
	}
}

func TestEgress_BadDest400(t *testing.T) {
	rig := newEgressRig(t, egressTestToken, okOrigin)
	for _, dest := range []string{"", "ftp://h:1", "h:1", "http://", "http://h", "http://h:1/path"} {
		req, _ := http.NewRequest(http.MethodGet, rig.egress.URL+"/", nil)
		req.Header.Set(headerToken, egressTestToken)
		req.Header.Set(headerDest, dest)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("dest %q: status = %d, want 400", dest, resp.StatusCode)
		}
	}
	if n := rig.hits.Load(); n != 0 {
		t.Fatalf("origin saw %d requests, want 0", n)
	}
}

func TestEgress_RelaysAndStripsHeaders(t *testing.T) {
	var got *http.Request
	rig := newEgressRig(t, egressTestToken, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = io.WriteString(w, "hello")
	})
	req, _ := http.NewRequest(http.MethodGet, rig.egress.URL+"/v1/x?a=b", nil)
	req.Host = "api.example.com"
	req.Header.Set(headerToken, egressTestToken)
	req.Header.Set(headerDest, rig.dest())
	req.Header.Set(headerEgressError, "upstream_dns")
	req.Header.Set("X-Cortex-Custom", "keep")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("got %d %q, want 200 hello", resp.StatusCode, body)
	}
	if got.URL.Path != "/v1/x" || got.URL.RawQuery != "a=b" {
		t.Errorf("origin URL = %s, want /v1/x?a=b", got.URL)
	}
	if got.Host != "api.example.com" {
		t.Errorf("origin Host = %q, want api.example.com", got.Host)
	}
	if got.Header.Get("X-Cortex-Custom") != "keep" {
		t.Errorf("X-Cortex-Custom did not reach the origin")
	}
	for _, h := range []string{headerDest, headerToken, headerEgressError} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("origin got %s = %q, want none", h, v)
		}
	}
}

func TestEgress_GzipPassesUnchanged(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte("compressed body"))
	_ = zw.Close()
	rig := newEgressRig(t, egressTestToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("origin Accept-Encoding = %q, want gzip", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(zipped.Bytes())
	})
	resp := egressDo(t, rig, "/", egressTestToken, rig.dest(), http.Header{"Accept-Encoding": {"gzip"}})
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(body, zipped.Bytes()) {
		t.Errorf("body changed in transit: got %d bytes, want %d", len(body), zipped.Len())
	}
}

func TestEgress_OriginEgressErrorRemoved(t *testing.T) {
	rig := newEgressRig(t, egressTestToken, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerEgressError, "upstream_dns")
		w.WriteHeader(http.StatusOK)
	})
	resp := egressDo(t, rig, "/", egressTestToken, rig.dest(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if v := resp.Header.Get(headerEgressError); v != "" {
		t.Fatalf("client got %s = %q, want none", headerEgressError, v)
	}
}

func TestEgress_DialFailure502WithKind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	rig := newEgressRig(t, egressTestToken, okOrigin)
	resp := egressDo(t, rig, "/", egressTestToken, closed, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if v := resp.Header.Get(headerEgressError); v != "upstream_refused" {
		t.Fatalf("%s = %q, want upstream_refused", headerEgressError, v)
	}
}

func TestEgress_GuardRefusal403(t *testing.T) {
	refusing := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &egressguard.Refusal{Host: "10.0.0.1", Addr: netip.MustParseAddr("10.0.0.1")}
	}}
	egress := httptest.NewServer(NewEgressHandler(egressTestToken, refusing))
	t.Cleanup(egress.Close)
	req, _ := http.NewRequest(http.MethodGet, egress.URL+"/", nil)
	req.Header.Set(headerToken, egressTestToken)
	req.Header.Set(headerDest, "http://10.0.0.1:80")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if v := resp.Header.Get(headerEgressError); v != "destination_private" {
		t.Fatalf("%s = %q, want destination_private", headerEgressError, v)
	}
}

func TestEgress_StreamsFrames(t *testing.T) {
	release := make(chan struct{})
	rig := newEgressRig(t, egressTestToken, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	defer close(release)
	resp := egressDo(t, rig, "/", egressTestToken, rig.dest(), nil)
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if !strings.HasPrefix(s, "data: one") {
			t.Fatalf("first frame = %q, want data: one", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first frame did not arrive before the origin finished")
	}
}
