package forwardproxy

import (
	"io/fs"
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
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".jsonl.zst") {
			segs = append(segs, p)
		}
		return nil
	})
	if len(segs) != 0 {
		t.Fatalf("segments on disk %v, want none", segs)
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
