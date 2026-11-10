package attribution

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Strip removes every attribution header and nothing else.
func TestWire_StripRemovesOnlyAttribution(t *testing.T) {
	h := http.Header{}
	for _, k := range wireHeaders {
		h.Set(k, "forged")
	}
	h.Set("User-Agent", "curl/8")
	Strip(h)
	if len(h) != 1 || h.Get("User-Agent") != "curl/8" {
		t.Errorf("after Strip: %v, want only User-Agent", h)
	}
}

var update = flag.Bool("update", false, "rewrite testdata/wire.golden from Wire.Encode")

// golden is the Wire that testdata/wire.golden holds. The Rust test decodes the file and
// compares it with these values.
var golden = Wire{Agent: "claude-code", Proc: "p1", ClientProc: "p2", Session: "s1", Source: "header",
	SessionHeader: "X-Claude-Code-Session-Id", ChainSession: "s0", RecordSession: "s1"}

// The encoding of golden must equal the file that the gateway's decoder test reads.
func TestWire_EncodeGolden(t *testing.T) {
	path := filepath.Join("testdata", "wire.golden")
	got := golden.Encode()
	if *update {
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("Encode() = %q, want %q", got, want)
	}
}

// The JSON keys must be the camelCase names of the Rust Attribution struct.
func TestWire_KeysMatchRust(t *testing.T) {
	b, err := json.Marshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"agent", "chainSession", "clientProc", "proc", "recordSession", "session", "sessionHeader", "source"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
}

// The Rust decoder reads base64url with no padding.
func TestWire_EncodeIsUnpadded(t *testing.T) {
	for _, w := range []Wire{golden, {Agent: "a"}, {Agent: "ab"}, {}} {
		if s := w.Encode(); strings.Contains(s, "=") {
			t.Errorf("Encode(%+v) = %q has padding", w, s)
		}
	}
}
