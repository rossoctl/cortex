package inference

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// infoOnly is the production default level, Info, with no output. Debug is off, Info
// is on, so a guard on the wrong level fails the test. A text handler is not used:
// under -race its buffer pool drops items at random and changes the allocation count.
type infoOnly struct{ slog.Handler }

func (infoOnly) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

// With Debug off, message text must not reach the log sanitizer: it allocates for
// a control rune, and every real prompt has one ("\n"). U+0085 is a C1 control that
// JSON carries unescaped, so it and "é" (same byte length) decode the same way and
// differ only in what the sanitizer does with them.
func TestParseRequest_DebugOffDoesNotSanitizeMessages(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(infoOnly{slog.DiscardHandler}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if !model.HasControlRunes("\u0085") || model.HasControlRunes("é") {
		t.Fatal("fixture runes no longer split control from plain")
	}
	body := func(r string) []byte {
		var b strings.Builder
		b.WriteString(`{"model":"m","messages":[`)
		for i := 0; i < 50; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"role":"user","content":"a` + r + `b"}`)
		}
		b.WriteString(`]}`)
		return []byte(b.String())
	}
	allocs := func(b []byte) float64 {
		return testing.AllocsPerRun(20, func() { ParseRequest("/v1/messages", http.Header{}, b) })
	}
	control, plain := allocs(body("\u0085")), allocs(body("é"))
	if control != plain {
		t.Errorf("allocs with a control rune = %v, without = %v; want equal with Debug off", control, plain)
	}
}

// The guard must not drop the lines it guards.
func TestParseRequest_DebugOnLogsMessages(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ParseRequest("/v1/messages", http.Header{}, []byte(`{"model":"m","messages":[{"role":"user","content":"hi\nthere"}]}`))
	if !strings.Contains(buf.String(), `msg="inference-parser: message" index=0 role=user content=`) {
		t.Errorf("no message line in the Debug log:\n%s", buf.String())
	}
}
