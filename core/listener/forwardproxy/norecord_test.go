package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
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
		got = append(got, r.Header.Get(model.RecordHeader))
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
	if len(*seen) != 1 || (*seen)[0] != "" {
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
	if len(*seen) != 1 || (*seen)[0] != "" {
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
