package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// decodeWireFixture decodes a fixture that the proxy's own tests check against the
// real handler. Unknown keys are an error, so a JSON tag renamed here fails. The
// decoded value, marshalled again, must equal the fixture.
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
		return
	}
	back, err := json.Marshal(into)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%T reads keys that %s does not hold, or holds them as other values. Add them to the fixture.\nfixture: %s\n%T: %s",
			into, path, raw, into, back)
	}
}

func TestStatsDecoders_ReadTheWireFixtures(t *testing.T) {
	decodeWireFixture(t, "../../core/reloader/testdata/reload-status.json", &edit.ReloadStatus{})
	decodeWireFixture(t, "../../core/observe/testdata/config.json", &runningConfigFields{})
	decodeWireFixture(t, "../../core/cost/pricing/testdata/pricing-table.json", &describeBody{})
	decodeWireFixture(t, "../../core/cost/pricing/testdata/pricing-effective.json", &effective{})
	decodeWireFixture(t, "../../core/tlsbridge/testdata/unread.json", &tlsbridge.UnreadReport{})
}
