package archive

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/rossoctl/cortex/core/pipeline"
)

// foldText is the search route's text check, in the form Find takes it.
func foldText(q string) func(string) bool {
	q = strings.Map(unicode.ToLower, q)
	return func(s string) bool { return strings.Contains(strings.Map(unicode.ToLower, s), q) }
}

func noEvent(*pipeline.SessionEvent) bool { return false }

// longText is a message content long enough to be a block of its own (blocks start at
// session.InternMinLen bytes).
func longText(s string) string { return s + " " + strings.Repeat("filler words ", 10) }

// reqCapture is a request capture whose body has one user message per text.
func reqCapture(texts ...string) *pipeline.RequestCapture {
	var msgs []string
	for _, t := range texts {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+t+`"}]}`)
	}
	body := `{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`
	return &pipeline.RequestCapture{Original: []byte(body),
		Header: http.Header{"X-Trace": {"trace-hdr-value"}}, Query: "beta=true"}
}

// findArchive writes the events, each with its capture, and closes the archive.
func findArchive(t *testing.T, caps ...*pipeline.RequestCapture) *Archive {
	t.Helper()
	a := openTest(t, t.TempDir(), newClock())
	a.inWriter(func() {
		for i, c := range caps {
			phase := pipeline.SessionRequest
			if c != nil && c.Response != nil {
				phase = pipeline.SessionResponse
			}
			a.write("s", &pipeline.SessionEvent{Seq: uint64(i + 1), SessionID: "s", Phase: phase}, c)
		}
		a.tick()
	})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return a
}

// Every request re-sends the conversation, so a prompt is one block that each later request
// uses. Each of those events matches.
func TestFind_ABlockMatchesEveryEventThatUsesIt(t *testing.T) {
	first := longText("find the NEEDLE-RAW")
	a := findArchive(t, reqCapture(first), reqCapture(first, longText("next turn")), reqCapture(longText("other")))
	seqs, last, ok, err := a.Find(context.Background(), "s", noEvent, foldText("needle-raw"))
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if !slices.Equal(seqs, []uint64{1, 2}) || last != 3 {
		t.Errorf("seqs %v last %d, want [1 2] and 3", seqs, last)
	}
}

// The detail view shows an image block and a binary body as labels, so their base64 or bytes
// must not match.
func TestFind_SkipsWhatTheDetailShowsAsALabel(t *testing.T) {
	img := `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("NEEDLEB64", 20) + `"}}]}`
	imgCap := &pipeline.RequestCapture{Original: []byte(`{"model":"m","messages":[` + img + `]}`)}
	binResp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200,
		Body: []byte("\xff\xfe NEEDLEB64 " + strings.Repeat("x", 100))}}
	textResp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200,
		Body: []byte("data: " + longText("the NEEDLEB64 reply"))}}
	a := findArchive(t, imgCap, binResp, textResp)

	seqs, _, _, err := a.Find(context.Background(), "s", noEvent, foldText("needleb64"))
	if err != nil || !slices.Equal(seqs, []uint64{3}) {
		t.Errorf("seqs %v err %v, want only the text response [3]", seqs, err)
	}
}

func TestFind_HeadersQueryAndStatus(t *testing.T) {
	resp := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 418,
		Header: http.Header{"X-Reply": {"teapot-hdr"}}}}
	a := findArchive(t, reqCapture(longText("hello")), resp)
	for q, want := range map[string][]uint64{
		"trace-hdr": {1}, "x-trace": {1}, "beta=true": {1}, "418": {2}, "teapot-hdr": {2},
	} {
		if seqs, _, _, _ := a.Find(context.Background(), "s", noEvent, foldText(q)); !slices.Equal(seqs, want) {
			t.Errorf("%q: seqs %v, want %v", q, seqs, want)
		}
	}
}

// event decides for the event's own fields, with no capture needed.
func TestFind_EventPredicate(t *testing.T) {
	a := findArchive(t, nil, nil, nil)
	seqs, _, _, _ := a.Find(context.Background(), "s",
		func(e *pipeline.SessionEvent) bool { return e.Seq == 2 }, foldText("absent"))
	if !slices.Equal(seqs, []uint64{2}) {
		t.Errorf("seqs %v, want [2]", seqs)
	}
}

func TestFind_UnknownSession(t *testing.T) {
	a := findArchive(t, nil)
	if _, _, ok, err := a.Find(context.Background(), "other", noEvent, foldText("x")); ok || err != nil {
		t.Errorf("ok %v err %v, want false and nil", ok, err)
	}
}

func TestFind_StopsWhenTheContextEnds(t *testing.T) {
	a := findArchive(t, reqCapture(longText("NEEDLE")), reqCapture(longText("NEEDLE")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seqs, _, _, err := a.Find(ctx, "s", noEvent, foldText("needle"))
	if err == nil || len(seqs) != 0 {
		t.Errorf("seqs %v err %v, want none and the context's error", seqs, err)
	}
}

// The writer still appends to the open segment. Its frame has not ended, which the reader
// reports as a torn tail: that ends the segment's read, not the search.
func TestFind_ReadsTheOpenSegment(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.inWriter(func() {
		a.write("s", &pipeline.SessionEvent{Seq: 1, SessionID: "s", Phase: pipeline.SessionRequest},
			reqCapture(longText("open NEEDLE")))
		a.tick()
	})
	seqs, _, ok, err := a.Find(context.Background(), "s", noEvent, foldText("needle"))
	if err != nil || !ok || !slices.Equal(seqs, []uint64{1}) {
		t.Errorf("seqs %v ok %v err %v, want [1]", seqs, ok, err)
	}
}
