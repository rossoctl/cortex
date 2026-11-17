package sessionapi

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/session/importer"
)

// importSource yields fixed sessions. When block is set, Sessions first waits until it closes.
// gap is a wait before each progress call after the first.
type importSource struct {
	sessions []importer.Session
	block    chan struct{}
	gap      time.Duration
}

func (f *importSource) Agent() string       { return "claude-code" }
func (f *importSource) PricingHost() string { return "api.anthropic.com" }
func (f *importSource) Sessions(_ time.Time, skip func(string, time.Time) bool, progress func(int, int)) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		if f.block != nil {
			<-f.block
		}
		defer progress(len(f.sessions), len(f.sessions))
		for i, s := range f.sessions {
			if i > 0 {
				time.Sleep(f.gap)
			}
			progress(i, len(f.sessions))
			if skip(s.ID, s.Updated) {
				continue
			}
			if !yield(s, nil) {
				return
			}
		}
	}
}

// importSession is id with one request/response pair, a minute ago.
func importSession(id string) importer.Session {
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	return importer.Session{ID: id, Updated: at, SizeHint: 1024, Events: []model.SessionEvent{
		{At: at, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: id + "-r0",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}},
		{At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse, RequestID: id + "-r0",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}},
	}}
}

// importServer is a session API with a temporary archive and the given sources.
func importServer(t *testing.T, srcs []importer.Source, opts ...Option) *httptest.Server {
	t.Helper()
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	opts = append([]Option{WithArchive(a), WithLoopbackOnly(true), WithImport(Import{Sources: srcs})}, opts...)
	ts := httptest.NewServer(New(":0", store, opts...).server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
		a.Close()
	})
	return ts
}

func postImport(t *testing.T, base, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(base+"/v1/import", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func decodeReport(t *testing.T, b []byte) importer.Report {
	t.Helper()
	var r importer.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("report %q: %v", b, err)
	}
	return r
}

// wantImported fails unless ?archived=true lists id with the imported mark.
func wantImported(t *testing.T, base, id string) {
	t.Helper()
	for _, s := range getList(t, base+"/v1/sessions?archived=true").Sessions {
		if s.ID == id {
			if s.Source != "imported:claude-code" {
				t.Fatalf("session %s has source %q", id, s.Source)
			}
			return
		}
	}
	t.Fatalf("?archived=true does not list %s", id)
}

func TestImport_RouteAbsentWithoutArchive(t *testing.T) {
	ts, _ := newTestServer(t, WithLoopbackOnly(true),
		WithImport(Import{Sources: []importer.Source{&importSource{}}}))
	if code, _ := postImport(t, ts.URL, `{}`); code != http.StatusNotFound {
		t.Fatalf("POST /v1/import = %d, want 404", code)
	}
}

func TestImport_RouteAbsentWhenNotLoopbackOnly(t *testing.T) {
	ts := importServer(t, []importer.Source{&importSource{}}, WithLoopbackOnly(false))
	if code, _ := postImport(t, ts.URL, `{}`); code != http.StatusNotFound {
		t.Fatalf("POST /v1/import = %d, want 404", code)
	}
}

func TestImport_RunsAndReports(t *testing.T) {
	ts := importServer(t, []importer.Source{&importSource{sessions: []importer.Session{importSession("s1")}}})
	code, b := postImport(t, ts.URL, `{"agent":"claude-code"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/import = %d %s", code, b)
	}
	if r := decodeReport(t, b); len(r.Agents) != 1 || r.Agents[0].Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	wantImported(t, ts.URL, "s1")
}

// With "progress": true, Progress lines come before the report line. The first line is
// 0/total and the last is total/total. A line between them passes only when importProgressEvery
// went by since the last line sent.
func TestImport_ProgressLinesBeforeTheReport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		every, gap time.Duration
		want       int
	}{
		{"middle line dropped", time.Hour, 0, 2},
		{"middle line sent", 50 * time.Millisecond, 100 * time.Millisecond, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(old time.Duration) { importProgressEvery = old }(importProgressEvery)
			importProgressEvery = tc.every
			ts := importServer(t, []importer.Source{&importSource{
				sessions: []importer.Session{importSession("s1"), importSession("s2")}, gap: tc.gap}})
			code, b := postImport(t, ts.URL, `{"progress":true}`)
			if code != http.StatusOK {
				t.Fatalf("POST /v1/import = %d %s", code, b)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			var got []importer.Progress
			for _, l := range lines[:len(lines)-1] {
				var p struct{ Progress importer.Progress }
				if err := json.Unmarshal([]byte(l), &p); err != nil {
					t.Fatalf("line %q: %v", l, err)
				}
				got = append(got, p.Progress)
			}
			first, last := importer.Progress{Agent: "claude-code", Done: 0, Total: 2}, importer.Progress{Agent: "claude-code", Done: 2, Total: 2}
			if len(got) != tc.want || got[0] != first || got[len(got)-1] != last {
				t.Fatalf("progress = %+v, want %d lines, %v first, %v last", got, tc.want, first, last)
			}
			for i := 1; i < len(got); i++ {
				if got[i].Done <= got[i-1].Done {
					t.Fatalf("progress = %+v, want Done to grow", got)
				}
			}
			if r := decodeReport(t, []byte(lines[len(lines)-1])); len(r.Agents) != 1 || r.Agents[0].Imported != 2 {
				t.Fatalf("report = %+v, want 2 imported", r)
			}
		})
	}
}

func TestImport_UnknownAgentIs400(t *testing.T) {
	ts := importServer(t, []importer.Source{&importSource{}})
	code, b := postImport(t, ts.URL, `{"agent":"nope"}`)
	if code != http.StatusBadRequest || !strings.Contains(string(b), "claude-code") {
		t.Fatalf("POST /v1/import = %d %q, want 400 that names claude-code", code, b)
	}
	if code, _ := postImport(t, ts.URL, `{`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d, want 400", code)
	}
}

// The body has no path field: an extra "path" key is ignored, and the configured source runs.
func TestImport_PathFieldIgnored(t *testing.T) {
	ts := importServer(t, []importer.Source{&importSource{sessions: []importer.Session{importSession("s1")}}})
	code, b := postImport(t, ts.URL, `{"path":"/etc"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/import = %d %s", code, b)
	}
	if r := decodeReport(t, b); len(r.Agents) != 1 || r.Agents[0].Imported != 1 || r.Agents[0].Error != "" {
		t.Fatalf("report = %+v, want the configured source only", r)
	}
}

func TestImport_SecondRequestIs409(t *testing.T) {
	src := &importSource{sessions: []importer.Session{importSession("s1")}, block: make(chan struct{})}
	ts := importServer(t, []importer.Source{src})
	// Registered after the server, so it runs first: a failed check must not leave the handler
	// blocked while the server's cleanup waits for it.
	var once sync.Once
	unblock := func() { once.Do(func() { close(src.block) }) }
	t.Cleanup(unblock)
	resp, err := http.Post(ts.URL+"/v1/import", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The 200 header comes before the work, so the first import holds the lock now.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first POST = %d", resp.StatusCode)
	}
	if code, _ := postImport(t, ts.URL, `{}`); code != http.StatusConflict {
		t.Fatalf("second POST = %d, want 409", code)
	}
	unblock()
	b, _ := io.ReadAll(resp.Body)
	if r := decodeReport(t, b); len(r.Agents) != 1 || r.Agents[0].Imported != 1 {
		t.Fatalf("first report = %+v, want 1 imported", r)
	}
}

// Decision 12: an erase leaves no tombstone, so the next import writes the session again.
func TestImport_ErasedSessionComesBack(t *testing.T) {
	ts := importServer(t, []importer.Source{&importSource{sessions: []importer.Session{importSession("s1")}}},
		WithClearAllowed(true))
	if code, b := postImport(t, ts.URL, `{}`); code != http.StatusOK || decodeReport(t, b).Agents[0].Imported != 1 {
		t.Fatalf("first import = %d %s", code, b)
	}
	if code, b := doDelete(t, ts.URL, "s1"); code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	code, b := postImport(t, ts.URL, `{}`)
	if r := decodeReport(t, b); code != http.StatusOK || r.Agents[0].Imported != 1 {
		t.Fatalf("second import = %d %s, want 1 imported", code, fmt.Sprint(r))
	}
	wantImported(t, ts.URL, "s1")
}
