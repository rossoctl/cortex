package mcp

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// With Debug off, the body must not reach the log sanitizer. See
// inference.TestParseRequest_DebugOffDoesNotSanitizeMessages for the fixture runes.
func TestParseRequest_DebugOffDoesNotSanitizeBody(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if !model.HasControlRunes("\u0085") || model.HasControlRunes("é") {
		t.Fatal("fixture runes no longer split control from plain")
	}
	body := func(r string) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"s":"a` + r + `b"}}}`)
	}
	allocs := func(b []byte) float64 {
		return testing.AllocsPerRun(20, func() { ParseRequest(http.MethodPost, "/mcp", http.Header{}, b, nil) })
	}
	control, plain := allocs(body("\u0085")), allocs(body("é"))
	if control != plain {
		t.Errorf("allocs with a control rune = %v, without = %v; want equal with Debug off", control, plain)
	}
}

// The guard must not drop the line it guards.
func TestParseRequest_DebugOnLogsBody(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ParseRequest(http.MethodPost, "/mcp", http.Header{}, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), nil)
	if !strings.Contains(buf.String(), `msg="mcp-parser: payload" method=tools/list body=`) {
		t.Errorf("no payload line in the Debug log:\n%s", buf.String())
	}
}
