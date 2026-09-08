package sessionapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func getPath(t *testing.T, base, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp, string(body)
}

// GET / must answer, not 404. A 404 there gives no way to tell "wrong port" from
// "right port, no route" — and the port is often assigned dynamically, so an
// operator running curl against it has nothing confirming they found the session
// API at all.
func TestHandleIndex_IdentifiesTheAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := getPath(t, ts.URL, "/")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(body, "Session API") {
		t.Errorf("body does not identify the API:\n%s", body)
	}
	// The trust model belongs here: this is the one page a stranger to the port
	// will read, and the payloads carry request and response bodies.
	if !strings.Contains(strings.ToLower(body), "unauthenticated") {
		t.Errorf("index does not state the trust model:\n%s", body)
	}
}

// Every route the server registers must be listed, or the index becomes a stale
// map of the API — worse than none, since a reader would trust it.
func TestHandleIndex_ListsEveryRegisteredRoute(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := getPath(t, ts.URL, "/")

	for _, route := range []string{
		"/v1/sessions",
		"/v1/sessions/{id}",
		"/v1/events",
		"/v1/pipeline",
		"/v1/plugins",
		"/v1/usage",
		"/healthz",
	} {
		if !strings.Contains(body, route) {
			t.Errorf("index omits %s:\n%s", route, body)
		}
	}
}

// Registered as "/{$}", not "/". Go's "/" is a catch-all, which would turn every
// genuine 404 into the index page and hide a typo like /v1/session.
func TestHandleIndex_DoesNotSwallow404s(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/nope", "/v1/session", "/v1/", "/v1/sessions/x/y"} {
		resp, body := getPath(t, ts.URL, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 — the index must match only /",
				path, resp.StatusCode)
		}
		if strings.Contains(body, "Session API") {
			t.Errorf("GET %s served the index instead of 404ing", path)
		}
	}
}

// The index is one line per endpoint, per the issue: short enough to read in a
// terminal without scrolling.
func TestHandleIndex_StaysShort(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := getPath(t, ts.URL, "/")

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > 16 {
		t.Errorf("index is %d lines; keep it terminal-sized", len(lines))
	}
	for _, l := range lines {
		if len(l) > 80 {
			t.Errorf("line exceeds 80 columns (%d):\n%q", len(l), l)
		}
	}
}

// Reachable regardless of which optional endpoints are wired: the index is how an
// operator identifies the port, so it must not depend on WithUsage/WithCatalog.
func TestHandleIndex_WorksWithNoOptionalEndpoints(t *testing.T) {
	ts, _ := newTestServer(t) // no WithUsage, no WithCatalog
	if resp, _ := getPath(t, ts.URL, "/"); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d with no optional endpoints wired, want 200", resp.StatusCode)
	}
}
