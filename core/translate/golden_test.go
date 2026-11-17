package translate

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

// requestGolden gives the golden form of one request conversion: the
// first line names the dropped fields, the second line is the body. A
// refusal gives one "error:" line.
func requestGolden(in []byte) []byte {
	c, err := Request(Anthropic, Chat, in, nil)
	if err != nil {
		return []byte("error: " + err.Error() + "\n")
	}
	return []byte("dropped: " + strings.Join(c.Dropped, ",") + "\n" + string(c.Body) + "\n")
}

// TestRequestGolden converts each hand-written Anthropic request in
// testdata/request to chat completions (rules 11-19, 20a).
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

func encodeChat(t *testing.T, in []byte) string {
	t.Helper()
	c, err := Request(Anthropic, Chat, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(c.Body)
}

// TestDeterministic checks rule 9a on the full multi-turn request.
func TestDeterministic(t *testing.T) {
	in, err := os.ReadFile("testdata/request/full_multiturn.json")
	if err != nil {
		t.Fatal(err)
	}
	first := encodeChat(t, in)
	if again := encodeChat(t, in); again != first {
		t.Fatalf("two encodings differ:\n%s\n%s", first, again)
	}

	// One more turn: the bytes up to the end of the old last message stay.
	const marker = `,"cache_control":{"type":"ephemeral"}`
	const oldLast = `"text":"Thanks. Is 391 prime?"`
	lastOut := `{"role":"user","content":[{"type":"text",` + oldLast + marker + `}]}`
	end := strings.Index(first, lastOut)
	if end < 0 {
		t.Fatalf("the last old message is not in the output:\n%s", first)
	}
	end += len(lastOut)
	lastIn := oldLast + marker + `}]}`
	longer := bytes.Replace(in, []byte(lastIn), []byte(lastIn+
		`,{"role":"assistant","content":[{"type":"text","text":"No: 17 x 23."}]}`+
		`,{"role":"user","content":[{"type":"text","text":"And 397?"}]}`), 1)
	if bytes.Equal(longer, in) {
		t.Fatal("the fixture does not hold the last user turn")
	}
	grown := encodeChat(t, longer)
	if !strings.HasPrefix(grown, first[:end]) {
		t.Errorf("one more turn changed the old bytes:\n old %s\n new %s", first[:end], grown)
	}

	// Move the cache marker to the new last turn: only the marker bytes
	// change.
	moved := bytes.Replace(longer, []byte(oldLast+marker), []byte(oldLast), 1)
	moved = bytes.Replace(moved, []byte(`"text":"And 397?"`), []byte(`"text":"And 397?"`+marker), 1)
	movedOut := encodeChat(t, moved)
	if movedOut == grown {
		t.Fatal("moving the marker did not change the output")
	}
	if strings.ReplaceAll(movedOut, marker, "") != strings.ReplaceAll(grown, marker, "") {
		t.Errorf("moving the marker changed more than the marker:\n%s\n%s", grown, movedOut)
	}
}

// replyCase is one hand-written chat-completions reply in testdata/reply.
// A .sse input is a stream. The status is 200, or N for a name that
// starts with "error_N_".
type replyCase struct {
	path   string
	status int
	stream bool
}

func replyCases(t *testing.T) []replyCase {
	t.Helper()
	inputs, _ := filepath.Glob("testdata/reply/*.sse")
	more, _ := filepath.Glob("testdata/reply/*.json")
	inputs = append(inputs, more...)
	if len(inputs) == 0 {
		t.Fatal("no reply inputs")
	}
	var out []replyCase
	for _, in := range inputs {
		c := replyCase{path: in, status: 200, stream: strings.HasSuffix(in, ".sse")}
		if rest, ok := strings.CutPrefix(filepath.Base(in), "error_"); ok {
			n, err := strconv.Atoi(strings.SplitN(rest, "_", 2)[0])
			if err != nil {
				t.Fatalf("%s: no status in the name", in)
			}
			c.status = n
		}
		out = append(out, c)
	}
	return out
}

// agentForms gives the reply shapes that the agent asks for: the same
// shape as the upstream for a 2xx reply, both shapes for an error.
func (c replyCase) agentForms() []bool {
	if c.status >= 200 && c.status < 300 {
		return []bool{c.stream}
	}
	return []bool{true, false}
}

func goldenName(c replyCase, agentStream bool) string {
	if agentStream {
		return c.path + ".to-sse.golden"
	}
	return c.path + ".to-json.golden"
}

// TestReplyGolden converts each chat-completions reply to the Anthropic
// reply (rules 20-24).
func TestReplyGolden(t *testing.T) {
	for _, c := range replyCases(t) {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, agentStream := range c.agentForms() {
			name := goldenName(c, agentStream)
			t.Run(filepath.Base(name), func(t *testing.T) {
				checkGolden(t, name, Reply(Anthropic, Chat, c.status, c.stream, agentStream, body))
			})
		}
	}
}
