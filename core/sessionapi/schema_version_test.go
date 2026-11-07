package sessionapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Each JSON response that carries events has one top-level schemaVersion. The events
// in it do not carry one each.
func TestEventResponses_CarryTheSchemaVersion(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	for _, path := range []string{"/v1/sessions/s1", "/v1/sessions/s1?view=summary", "/v1/sessions/s1/events/1"} {
		code, body := getBody(t, ts.URL+path)
		var got struct {
			SchemaVersion int              `json:"schemaVersion"`
			Events        []map[string]any `json:"events"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		if got.SchemaVersion != SchemaVersion {
			t.Errorf("GET %s: schemaVersion = %d, want %d: %s", path, got.SchemaVersion, SchemaVersion, body)
		}
		for _, ev := range got.Events {
			if _, ok := ev["schemaVersion"]; ok {
				t.Errorf("GET %s: an event carries its own schemaVersion: %s", path, body)
			}
		}
	}
}
