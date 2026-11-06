package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/rossoctl/cortex/core/tlsbridge"
)

// decodeWireFixture decodes a fixture that the proxy's own tests check against the
// real handler. Unknown keys are an error, so a JSON tag renamed here fails.
func decodeWireFixture(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Errorf("%s does not decode into %T: %v", path, into, err)
	}
}

func TestStatsDecoders_ReadTheWireFixtures(t *testing.T) {
	decodeWireFixture(t, "../../core/observe/testdata/config.json", &runningConfigFields{})
	decodeWireFixture(t, "../../core/cost/pricing/testdata/pricing-table.json", &describeBody{})
	decodeWireFixture(t, "../../core/cost/pricing/testdata/pricing-effective.json", &effective{})
	decodeWireFixture(t, "../../core/tlsbridge/testdata/unread.json", &tlsbridge.UnreadReport{})
}
