package a2a

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// infoOnly is the production default level, Info, with no output. A text handler is
// not used: under -race its buffer pool drops items at random and changes the
// allocation count. See inference.infoOnly.
type infoOnly struct{ slog.Handler }

func (infoOnly) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

// With Debug off, part text must not reach the log sanitizer. See
// inference.TestParseRequest_DebugOffDoesNotSanitizeMessages for the fixture runes.
func TestParseRequest_DebugOffDoesNotSanitizeParts(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(infoOnly{slog.DiscardHandler}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if !model.HasControlRunes("\u0085") || model.HasControlRunes("é") {
		t.Fatal("fixture runes no longer split control from plain")
	}
	body := func(r string) []byte {
		var b strings.Builder
		b.WriteString(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","messageId":"m","parts":[`)
		for i := 0; i < 50; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"kind":"text","text":"a` + r + `b"}`)
		}
		b.WriteString(`]}}}`)
		return []byte(b.String())
	}
	allocs := func(b []byte) float64 {
		return testing.AllocsPerRun(20, func() { ParseRequest("/", http.Header{}, b) })
	}
	control, plain := allocs(body("\u0085")), allocs(body("é"))
	if control != plain {
		t.Errorf("allocs with a control rune = %v, without = %v; want equal with Debug off", control, plain)
	}
}

// The guard must not drop the lines it guards.
func TestParseRequest_DebugOnLogsParts(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ParseRequest("/", http.Header{}, []byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi\nthere"}]}}}`))
	if !strings.Contains(buf.String(), `msg="a2a-parser: part" index=0 kind=text content=`) {
		t.Errorf("no part line in the Debug log:\n%s", buf.String())
	}
}
