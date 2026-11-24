package usage

import (
	"encoding/json"
	"testing"
)

// A /v1/usage reply whose "project" is not a string decodes, and reads as "not applied".
func TestSnapshotDecodesNonStringProjectAsAbsent(t *testing.T) {
	for _, v := range []string{`5`, `1.5`, `{}`, `[]`, `true`, `null`} {
		var s Snapshot
		if err := json.Unmarshal([]byte(`{"project":`+v+`,"buckets":[]}`), &s); err != nil {
			t.Errorf("project=%s: %v", v, err)
			continue
		}
		if s.Project != "" {
			t.Errorf("project=%s: Project = %q, want empty", v, s.Project)
		}
	}
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"project":"/repo/a","buckets":[]}`), &s); err != nil || s.Project != "/repo/a" {
		t.Errorf("string project: %q, %v", s.Project, err)
	}
}
