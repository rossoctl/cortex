package sessionapi

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func doDelete(t *testing.T, base, id string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, base+"/v1/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// The per-session route changes state on the same unauthenticated API as DELETE /v1/sessions,
// so a page the user visits can reach it the same ways: each of the four guards is pinned again.
// The Host variants are httpx.LoopbackHost's, already pinned in clear_test.go.
func TestHandleDelete_Guards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		method  string
		edit    func(*http.Request)
		want    int
	}{
		{"loopback, no origin", true, http.MethodDelete, nil, http.StatusOK},
		{"a rebound name", true, http.MethodDelete, func(r *http.Request) { r.Host = "evil.example:9094" }, http.StatusForbidden},
		{"a browser's origin", true, http.MethodDelete, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"not bound to loopback only", false, http.MethodDelete, nil, http.StatusForbidden},
		{"POST, which needs no preflight", true, http.MethodPost, nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store := newTestServer(t, WithClearAllowed(tc.allowed))
			store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
			store.Append("s2", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
			req, _ := http.NewRequest(tc.method, ts.URL+"/v1/sessions/s1", nil)
			if tc.edit != nil {
				tc.edit(req)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s = %d %s, want %d", tc.method, resp.StatusCode, body, tc.want)
			}
			if gone := store.View("s1") == nil; gone != (tc.want == http.StatusOK) {
				t.Fatalf("status %d but erased=%v", resp.StatusCode, gone)
			}
			if store.View("s2") == nil {
				t.Fatal("erasing s1 erased s2 too")
			}
			if tc.want == http.StatusForbidden && decodeClear(t, body).Error == "" {
				t.Fatalf("a refusal without a reason: %s", body)
			}
		})
	}
}

// A memory-only proxy reports the one session it erased and nothing on disk. Ids that need
// escaping — a pending bucket's, one holding a slash — reach the handler intact.
func TestHandleDelete_ErasesFromMemoryByExactID(t *testing.T) {
	for _, id := range []string{"s1", "pending:bob-shell", "a/b"} {
		ts, store := newTestServer(t, WithClearAllowed(true))
		store.Append(id, pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
		code, b := doDelete(t, ts.URL, id)
		if code != http.StatusOK {
			t.Fatalf("DELETE %q = %d %s", id, code, b)
		}
		if c := decodeClear(t, b); c.Sessions != 1 || c.ArchivedSessions != 0 || c.ArchiveError != "" {
			t.Fatalf("DELETE %q = %+v, want 1 in memory and nothing on disk", id, c)
		}
		if store.View(id) != nil {
			t.Fatalf("%q is still in the store", id)
		}
	}
}

// A session only the archive holds — the proxy restarted since — is erased from disk, after
// which it is neither listed nor served.
func TestHandleDelete_ErasesAnArchiveOnlySession(t *testing.T) {
	ts, _, _ := restarted(t, WithClearAllowed(true))
	code, b := doDelete(t, ts.URL, "s1")
	if code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	if c := decodeClear(t, b); c.Sessions != 0 || c.ArchivedSessions != 1 || c.Bytes <= 0 || c.ArchiveError != "" {
		t.Fatalf("DELETE = %+v, want 0 in memory, 1 on disk with its bytes", c)
	}
	if lb := getList(t, ts.URL+"/v1/sessions?archived=true"); len(lb.Sessions) != 1 || lb.Sessions[0].ID != "old" {
		t.Fatalf("listed after the delete: %+v, want old alone", lb.Sessions)
	}
	if code, _ := getBody(t, ts.URL+"/v1/sessions/s1"); code != http.StatusNotFound {
		t.Fatalf("GET the erased session = %d, want 404", code)
	}
}

// Neither half holding the id is a 404, with an archive and without.
func TestHandleDelete_AnUnknownIDIsNotFound(t *testing.T) {
	withArchive, _, _ := restarted(t, WithClearAllowed(true))
	memoryOnly, _ := newTestServer(t, WithClearAllowed(true))
	for _, base := range []string{withArchive.URL, memoryOnly.URL} {
		if code, b := doDelete(t, base, "never"); code != http.StatusNotFound {
			t.Fatalf("DELETE never = %d %s, want 404", code, b)
		}
	}
}

// Memory is erased whatever the disk does — it already was, under the store's lock — so a
// failure on disk is a 500 that still reports the memory half, plus why the disk half failed.
func TestHandleDelete_ArchiveErrorIs500WithTheMemoryCount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	ts, store, a := restarted(t, WithClearAllowed(true))
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if err := os.Chmod(a.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(a.Root(), 0o700) })
	code, b := doDelete(t, ts.URL, "live")
	c := decodeClear(t, b)
	if code != http.StatusInternalServerError || c.Sessions != 1 || c.ArchiveError == "" {
		t.Fatalf("DELETE = %d %+v, want 500 with the memory count and an archiveError", code, c)
	}
	if store.View("live") != nil {
		t.Fatal("memory was not erased")
	}
}
