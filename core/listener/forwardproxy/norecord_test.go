package forwardproxy

import (
	"bufio"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// row is what a recorder saw of one event.
type row struct {
	session  string
	phase    pipeline.SessionPhase
	noRecord bool
}

// rows records every event's session, phase and NoRecord.
type rows struct {
	mu  sync.Mutex
	got []row
}

func (r *rows) Record(sid string, e *pipeline.SessionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, row{sid, e.Phase, e.NoRecord})
}

func (r *rows) take() []row {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

func noRecordProxy(t *testing.T) (proxy *httptest.Server, backendURL string, rec *rows, seen *[]string) {
	t.Helper()
	var got []string
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// "<absent>" when the header is not there at all, not just empty.
		v := "<absent>"
		if vs := r.Header.Values(model.RecordHeader); len(vs) > 0 {
			v = strings.Join(vs, ",")
		}
		got = append(got, v)
		mu.Unlock()
	}))
	t.Cleanup(backend.Close)
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	rec = &rows{}
	store.AddRecorder(rec)
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader}}
	proxy = httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy, backend.URL, rec, &got
}

// A request that names its own session and carries X-Cortex-Record: off is forwarded without the
// header, and both of its rows carry NoRecord.
func TestNoRecord_OwnSessionIsWithheldAndHeaderStripped(t *testing.T) {
	proxy, backend, rec, seen := noRecordProxy(t)
	postThrough(t, proxy, backend+"/v1/messages", `{}`, http.Header{
		model.RecordHeader:              {"off"},
		session.ClaudeCodeSessionHeader: {"11111111-2222-3333-4444-555555555555"},
	})
	if len(*seen) != 1 || (*seen)[0] != "<absent>" {
		t.Fatalf("upstream saw %s = %q, want it stripped", model.RecordHeader, *seen)
	}
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("rows %+v, want a request and a response", got)
	}
	for _, r := range got {
		if !r.noRecord || r.session != "11111111-2222-3333-4444-555555555555" {
			t.Fatalf("row %+v, want NoRecord under the request's own session", r)
		}
	}
}

// A request that names no session is filed by fallback (the active session or the default
// bucket), so its flag must not withhold that bucket: it is recorded as usual. The header is
// still stripped.
func TestNoRecord_FallbackFiledRequestIsRecorded(t *testing.T) {
	proxy, backend, rec, seen := noRecordProxy(t)
	postThrough(t, proxy, backend+"/v1/messages", `{}`, http.Header{model.RecordHeader: {"off"}})
	if len(*seen) != 1 || (*seen)[0] != "<absent>" {
		t.Fatalf("upstream saw %s = %q, want it stripped", model.RecordHeader, *seen)
	}
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("rows %+v, want a request and a response", got)
	}
	for _, r := range got {
		if r.noRecord {
			t.Fatalf("row %+v under a fallback bucket carries NoRecord", r)
		}
	}
}

// The normal laptop path for an inference host is the TLS bridge: the CONNECT names nothing, and
// the bridged tunnel's open row is filed with the first decrypted request. When that request
// withholds its session, the open row shares its fate, so the archive holds no segment at all
// for the session (#1923), not even the CONNECT row with its project.
func TestNoRecord_BridgedRequestLeavesNoSegment(t *testing.T) {
	root := t.TempDir()
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Closed explicitly below before the scan; this covers an early exit. A second Close is a
	// no-op.
	t.Cleanup(func() { _ = arch.Close() })
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.AddRecorder(arch)
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+target+"/v1/messages", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "11111111-2222-3333-4444-555555555555")
	req.Header.Set(model.RecordHeader, "off")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("bridged request status %d, want 200", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	open, next := openThen(t, store, "11111111-2222-3333-4444-555555555555")
	if !open.NoRecord || !next.NoRecord {
		t.Fatalf("open row NoRecord=%v, request row NoRecord=%v, want both", open.NoRecord, next.NoRecord)
	}
	if err := arch.Close(); err != nil {
		t.Fatal(err)
	}
	var segs []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // an unread subtree must fail the test, not pass it
		}
		if strings.HasSuffix(p, ".jsonl.zst") {
			segs = append(segs, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if len(segs) != 0 {
		t.Fatalf("segments on disk %v, want none", segs)
	}
}

// sendConnectWith is sendConnect with extra request headers on the CONNECT.
func sendConnectWith(t *testing.T, proxyAddr, target string, h http.Header) (net.Conn, *bufio.Reader) {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	for k, vs := range h {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := raw.Write([]byte(b.String())); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	return raw, br
}

// An opaque (not bridged) CONNECT that names its own session and sends off withholds that
// session: its open row files under the named session at once, so it carries the flag. When the
// row files under another session (the active one, with no pin), it carries none, so no other
// session is stopped.
func TestNoRecord_OpaqueConnectWithholdsOnlyItsOwnSession(t *testing.T) {
	const own = "11111111-2222-3333-4444-555555555555"
	hdr := http.Header{session.ClaudeCodeSessionHeader: {own}, model.RecordHeader: {"off"}}
	for _, tc := range []struct {
		name     string
		affinity bool   // pins the row to the session the CONNECT names
		active   string // a session active before the CONNECT, or ""
		filed    string // where the open row files
		want     bool
	}{
		{"files under its own session", true, "", own, true},
		{"files under another session", false, "sess-A", "sess-A", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			if tc.active != "" {
				store.Append(tc.active, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
			}
			s := connectServer(t, store, nil) // no bridge: an opaque tunnel
			s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
			s.ClientAffinity = tc.affinity
			proxyAddr, done := connectProxy(t, s)
			raw, br := sendConnectWith(t, proxyAddr, pingPongOrigin(t), hdr)
			pingPong(t, raw, br)
			waitDone(t, done)
			open, _ := onePair(t, store, tc.filed)
			if open.NoRecord != tc.want {
				t.Fatalf("open row under %s: NoRecord=%v, want %v", tc.filed, open.NoRecord, tc.want)
			}
		})
	}
}

// The CONNECT's opt-out compares the filing session with the id pinned before the gate plugins
// ran, not with a re-read of pctx.Headers: a plugin that removes or rewrites the session-id header
// must not make an own-session opt-out record its open row.
func TestNoRecord_OpaqueConnectIgnoresARewrittenHeader(t *testing.T) {
	const own = "11111111-2222-3333-4444-555555555555"
	for _, rewritten := range []string{"", "99999999-2222-3333-4444-555555555555"} {
		store := session.New(5*time.Minute, 100, 0)
		h := http.Header{session.ClaudeCodeSessionHeader: {own}, model.RecordHeader: {"off"}}
		pctx := &pipeline.Context{Headers: h}
		pctx.ResolveClient()
		pctx.SetAttribution("", "", session.ClaudeCodeSessionHeader)
		pctx.SetNamedSession(own)
		pctx.OutboundSessionID = own
		// A gate plugin rewrites the header after the pin.
		if rewritten == "" {
			h.Del(session.ClaudeCodeSessionHeader)
		} else {
			h.Set(session.ClaudeCodeSessionHeader, rewritten)
		}
		s := &Server{Sessions: store, ClientAffinity: true, SessionIDHeaders: []string{session.ClaudeCodeSessionHeader}}
		s.appendTunnelOpen(pctx, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
		v := store.View(own)
		if v == nil || len(v.Events) != 1 || !v.Events[0].NoRecord {
			t.Fatalf("header rewritten to %q: rows under %s %+v, want one flagged open row", rewritten, own, v)
		}
		store.Close()
	}
}

// A plugin's upstream headers (SetUpstreamHeaders, as the inference router gives a server's
// configured headers) never carry X-Cortex-Record upstream, in any case (#2005).
func TestNoRecord_UpstreamHeadersNeverSetTheRecordHeader(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "http://upstream.example/v1/messages", nil)
	applyUpstreamHeaders(r, http.Header{
		model.RecordHeader: {"off"},
		"x-cortex-record":  {"off"},
		"X-Tenant":         {"a"},
	})
	for k := range r.Header {
		if strings.EqualFold(k, model.RecordHeader) {
			t.Fatalf("the upstream request has %s: %v", k, r.Header)
		}
	}
	if r.Header.Get("X-Tenant") != "a" {
		t.Fatalf("other upstream headers were not set: %v", r.Header)
	}
}

// A request without the header is unchanged.
func TestNoRecord_UnflaggedRequestIsRecorded(t *testing.T) {
	proxy, backend, rec, _ := noRecordProxy(t)
	postThrough(t, proxy, backend+"/v1/messages", `{}`, http.Header{
		session.ClaudeCodeSessionHeader: {"11111111-2222-3333-4444-555555555555"},
	})
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("rows %+v, want a request and a response", got)
	}
	for _, r := range got {
		if r.noRecord {
			t.Fatalf("row %+v carries NoRecord with no header", r)
		}
	}
}
