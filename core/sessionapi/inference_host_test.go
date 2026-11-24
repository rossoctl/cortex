package sessionapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

func inferenceRequest(host string) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: host,
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}}
}

// listedRow settles a — it marks a resumed entry's start on its own goroutine — and returns id's
// row from url, which must satisfy ok.
func listedRow(t *testing.T, a *archive.Archive, url, id string, ok func(session.SessionSummary) bool) session.SessionSummary {
	t.Helper()
	settleArchive(t, a)
	sessions := getList(t, url).Sessions
	for _, s := range sessions {
		if s.ID == id && ok(s) {
			return s
		}
	}
	t.Fatalf("%s: no row for %s matched; list is %+v", url, id, sessions)
	return session.SessionSummary{}
}

// /v1/sessions carries inferenceHost for every row it serves: a resident session, a session
// resumed after a restart (from the archive's fold until its own next inference request), and a
// session only the archive holds.
func TestHandleList_CarriesInferenceHost(t *testing.T) {
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	prevStore.Append("s1", inferenceRequest("ete.example.com"))
	prevStore.Append("old", inferenceRequest("glm.example.com:8443"))
	prevStore.Close()
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
		a.Close()
	})

	// A tunnel row resumes s1 without moving its host.
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Tunnel: true,
		HTTPMethod: "CONNECT", Host: "api.anthropic.com:443"})
	resumed := listedRow(t, a, ts.URL+"/v1/sessions", "s1", func(s session.SessionSummary) bool { return s.EventCount == 2 })
	if resumed.InferenceHost != "ete.example.com" {
		t.Errorf("resumed s1: inferenceHost = %q, want the archive's ete.example.com", resumed.InferenceHost)
	}
	old := listedRow(t, a, ts.URL+"/v1/sessions?archived=true", "old", func(session.SessionSummary) bool { return true })
	if old.Resident == nil || *old.Resident || old.InferenceHost != "glm.example.com:8443" {
		t.Errorf("archive-only old: %+v, want resident false and inferenceHost glm.example.com:8443", old)
	}

	// The entry's own inference request wins over the history.
	store.Append("s1", inferenceRequest("glm.example.com:8443"))
	if got := listedRow(t, a, ts.URL+"/v1/sessions", "s1", func(s session.SessionSummary) bool { return s.EventCount == 3 }); got.InferenceHost != "glm.example.com:8443" {
		t.Errorf("s1 after its own inference request: inferenceHost = %q", got.InferenceHost)
	}
	if _, body := getBody(t, ts.URL+"/v1/sessions"); !strings.Contains(string(body), `"inferenceHost":"glm.example.com:8443"`) {
		t.Errorf("the wire does not carry inferenceHost: %s", body)
	}
}
