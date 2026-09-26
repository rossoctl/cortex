package sessionapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// getView fetches a snapshot and decodes it, failing on anything but 200.
func getView(t *testing.T, base, path string) *pipeline.SessionView {
	t.Helper()
	resp, err := http.Get(base + path) //nolint:noctx // test client
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	var v pipeline.SessionView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return &v
}

// newUncappedServer is newTestServer with the store's own event cap removed.
//
// The shared helper builds its store with maxEvents=100, which would cap every session
// below the response bound and hide exactly what these tests are about: the endpoint's
// behaviour when the STORE is unbounded, which is the default the binaries now pass.
func newUncappedServer(t *testing.T) (*httptest.Server, *session.Store) {
	t.Helper()
	store := session.New(0, 0, 100)
	srv := New(":0", store, WithHeartbeatInterval(50*time.Millisecond))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts, store
}

func seed(t *testing.T, store interface {
	Append(string, pipeline.SessionEvent)
}, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		store.Append(id, pipeline.SessionEvent{Host: fmt.Sprintf("h%04d", i)})
	}
}

// The default bound is what stops this endpoint from being asked for everything.
//
// It is not a nicety: with session.max_events unset the store holds every event a
// session ever produced, and one real session reached 5078 of them — 1.1GB of JSON,
// 17s to encode, against a client that gives up at 10s. Unbounded here meant the
// operator saw an empty timeline and the proxy burned 17s producing bytes for nobody.
func TestHandleGet_DefaultsToABoundedTail(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", defaultEventLimit+250)

	v := getView(t, ts.URL, "/v1/sessions/s1")
	if got := len(v.Events); got != defaultEventLimit {
		t.Errorf("unbounded request returned %d events, want the default %d", got, defaultEventLimit)
	}
	if got, want := v.TotalEvents, defaultEventLimit+250; got != want {
		t.Errorf("TotalEvents = %d, want %d", got, want)
	}
	// The tail, not the head: the newest event must be present.
	if last := v.Events[len(v.Events)-1].Host; last != fmt.Sprintf("h%04d", defaultEventLimit+249) {
		t.Errorf("last event = %q, want the newest", last)
	}
}

// A session smaller than the default is returned whole, and says nothing about limits —
// byte-identical to what the endpoint produced before it had any.
func TestHandleGet_SmallSessionIsUnchanged(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", 12)

	v := getView(t, ts.URL, "/v1/sessions/s1")
	if got := len(v.Events); got != 12 {
		t.Errorf("%d events, want all 12", got)
	}
	if v.TotalEvents != 0 {
		t.Errorf("TotalEvents = %d on an untruncated view, want 0", v.TotalEvents)
	}
}

func TestHandleGet_LimitParameter(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", 300)

	for _, tc := range []struct {
		query string
		want  int
		why   string
	}{
		{"?limit=5", 5, "an explicit limit is honoured"},
		{"?limit=1", 1, "one event is a legal ask"},
		{"?limit=300", 300, "a limit covering the session returns it whole"},
		{"?limit=99999", 300, "over-large clamps to maxEventLimit, then to what exists"},
		// These four fall back to defaultEventLimit, which is larger than this
		// 300-event session — so "the default was applied" reads as 300 here. That the
		// default BOUNDS a bigger session is what TestHandleGet_DefaultsToABoundedTail
		// pins; this case only proves the fallback is the default and not "unlimited"
		// or an error.
		{"?limit=0", 300, "zero is not unlimited; it falls back to the default"},
		{"?limit=-3", 300, "negative falls back to the default"},
		{"?limit=abc", 300, "unparseable falls back rather than 400 — this is a debugging surface"},
		{"?limit=", 300, "empty falls back"},
	} {
		v := getView(t, ts.URL, "/v1/sessions/s1"+tc.query)
		if got := len(v.Events); got != tc.want {
			t.Errorf("%s: %s → %d events, want %d", tc.query, tc.why, got, tc.want)
		}
	}
}

// The hard maximum is the half of this that protects the process rather than the
// client: no query may make one response arbitrarily large.
func TestHandleGet_ClampsAtTheHardMaximum(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", maxEventLimit+500)

	v := getView(t, ts.URL, fmt.Sprintf("/v1/sessions/s1?limit=%d", maxEventLimit*10))
	if got := len(v.Events); got != maxEventLimit {
		t.Errorf("asked for %d, got %d, want the cap %d", maxEventLimit*10, got, maxEventLimit)
	}
	if got, want := v.TotalEvents, maxEventLimit+500; got != want {
		t.Errorf("TotalEvents = %d, want %d", got, want)
	}
}
