package parsefixture

import (
	"bufio"
	"net/textproto"
	"strings"
	"testing"
)

// TestFixtureReadersWithoutBlankLine checks the readers on files that end-of-file-fixer has changed.
func TestFixtureReadersWithoutBlankLine(t *testing.T) {
	got := sseFrames([]byte("event: a\ndata: 1\n\nevent: b\ndata: 2\n"))
	if len(got) != 2 || string(got[1]) != "2" {
		t.Errorf("sseFrames = %q, want the last event too", got)
	}
	for _, head := range []string{"Content-Type: x\n", "Content-Type: x\n\n"} {
		h, err := textproto.NewReader(bufio.NewReader(strings.NewReader(string(endHead([]byte(head)))))).ReadMIMEHeader()
		if err != nil || h.Get("Content-Type") != "x" {
			t.Errorf("head %q: %v, %v", head, h, err)
		}
	}
}
