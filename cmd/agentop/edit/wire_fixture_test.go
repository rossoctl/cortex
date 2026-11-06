package edit

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The proxy's own test checks this fixture against the real /reload/status handler.
// Unknown keys are an error, so a JSON tag renamed here fails.
func TestReloadStatus_ReadsTheWireFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../core/reloader/testdata/reload-status.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rs ReloadStatus
	if err := dec.Decode(&rs); err != nil {
		t.Errorf("fixture does not decode into ReloadStatus: %v", err)
	}
}
