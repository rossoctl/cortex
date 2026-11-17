package translate

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rossoctl/cortex/core/translate/anthropic"
	"github.com/rossoctl/cortex/core/translate/chat"
)

func newReader(r io.Reader, status int, upstreamStream, agentStream bool) io.ReadCloser {
	return NewReplyReader(io.NopCloser(r),
		chat.Protocol{}.NewReplyDecoder(status, upstreamStream),
		anthropic.Protocol{}.NewReplyEncoder(agentStream))
}

// TestReplyReader_EveryByteSplit reads each reply input one byte per
// Read, so the input splits at every byte boundary. The output must equal
// the golden.
func TestReplyReader_EveryByteSplit(t *testing.T) {
	for _, c := range replyCases(t) {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, agentStream := range c.agentForms() {
			name := goldenName(c, agentStream)
			t.Run(filepath.Base(name), func(t *testing.T) {
				want, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(newReader(iotest.OneByteReader(bytes.NewReader(body)), c.status, c.stream, agentStream))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("got %s\nwant %s", got, want)
				}
			})
		}
	}
}

// TestReplyReader_Incremental checks rule 10: the first chunk gives the
// message_start frame while the upstream stream is still open.
func TestReplyReader_Incremental(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		_, _ = pw.Write([]byte(`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"))
	}()
	r := newReader(pr, 200, true, true)
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := r.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.HasPrefix(s, "event: message_start\n") {
			t.Errorf("first Read = %q, want the message_start frame", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read waited for more of the stream")
	}
}

// TestReplyReader_ReadError checks that an upstream read error comes
// after the bytes already converted, and that Close closes the upstream.
func TestReplyReader_ReadError(t *testing.T) {
	boom := errors.New("boom")
	up := io.MultiReader(strings.NewReader(`data: {"id":"c","model":"m","choices":[]}`+"\n\n"), iotest.ErrReader(boom))
	closed := false
	r := NewReplyReader(closer{up, &closed},
		chat.Protocol{}.NewReplyDecoder(200, true), anthropic.Protocol{}.NewReplyEncoder(true))
	got, err := io.ReadAll(r)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if !strings.HasPrefix(string(got), "event: message_start\n") {
		t.Errorf("bytes before the error = %q", got)
	}
	if err := r.Close(); err != nil || !closed {
		t.Errorf("Close = %v, closed = %v", err, closed)
	}
}

type closer struct {
	io.Reader
	closed *bool
}

func (c closer) Close() error { *c.closed = true; return nil }
