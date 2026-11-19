package responses

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/chat"
)

var update = flag.Bool("update", false, "rewrite golden files")

// checkGolden compares got to the golden file, or writes it with -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", path, got, want)
	}
}

// requestGolden gives the golden form of one Responses request sent to a
// Chat server: the first line names the dropped fields, the second line
// is the body. A refusal gives one "error:" line.
func requestGolden(in []byte) []byte {
	req, err := Protocol{}.DecodeRequest(in)
	if err != nil {
		return []byte("error: " + err.Error() + "\n")
	}
	body, dropped, err := chat.Protocol{}.EncodeRequest(req, nil)
	if err != nil {
		return []byte("error: " + err.Error() + "\n")
	}
	return []byte("dropped: " + strings.Join(dropped, ",") + "\n" + string(body) + "\n")
}

// TestRequestGolden converts each hand-written Responses request in
// testdata/request to chat completions (rules R5-R10, R14).
func TestRequestGolden(t *testing.T) {
	inputs, _ := filepath.Glob("testdata/request/*.json")
	if len(inputs) == 0 {
		t.Fatal("no request inputs")
	}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, strings.TrimSuffix(in, ".json")+".golden", requestGolden(body))
		})
	}
}
