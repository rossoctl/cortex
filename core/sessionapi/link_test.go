package sessionapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// GET /v1/sessions links a process's pending bucket to the session it named, on the wire.
func TestHandleList_LinksAPendingBucketToItsSession(t *testing.T) {
	ts, store := newTestServer(t)
	bucket := session.PendingID("claude-code", "100.5")
	t0 := time.Now()
	store.Append(bucket, pipeline.SessionEvent{At: t0, Phase: pipeline.SessionRequest, Proc: "100.5"})
	store.Append("s1", pipeline.SessionEvent{At: t0.Add(time.Second), Phase: pipeline.SessionRequest,
		Proc: "100.5", SessionHeader: session.ClaudeCodeSessionHeader})

	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []struct {
			ID        string                `json:"id"`
			Procs     []string              `json:"procs"`
			LinkedTo  []session.PendingLink `json:"linkedTo"`
			LinkedAll bool                  `json:"linkedAll"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var sawBucket, sawSession bool
	for _, s := range body.Sessions {
		switch s.ID {
		case bucket:
			sawBucket = true
			if len(s.LinkedTo) != 1 || s.LinkedTo[0].Session != "s1" || !s.LinkedAll {
				t.Errorf("bucket linkedTo %+v linkedAll %v, want s1, all", s.LinkedTo, s.LinkedAll)
			}
		case "s1":
			sawSession = true
			if len(s.Procs) != 1 || s.Procs[0] != "100.5" {
				t.Errorf("s1 procs = %v, want [100.5]", s.Procs)
			}
		}
	}
	if !sawBucket || !sawSession {
		t.Fatalf("list = %+v", body.Sessions)
	}
}
