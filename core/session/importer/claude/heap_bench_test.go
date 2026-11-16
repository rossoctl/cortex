package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// BenchmarkImportHeap reports the heap that reading one long transcript costs the proxy.
// Each request event holds the whole conversation, so a parse that interns only after the
// merge peaks at O(turns^2). Measured on an M-series laptop: 628 MB HeapSys and 7.6 MB retained
// before events were interned as they are made, 16 MB and 2.6 MB after.
//
// HeapSys is the peak the allocator reached, which Go keeps as idle heap; HeapAlloc after
// GC is what the session retains. Reported rather than asserted, for the reason
// session.BenchmarkRetainedHeap gives. Run it in its own process:
//
//	go test ./session/importer/claude/ -bench ImportHeap -run '^$' -benchtime 1x
func BenchmarkImportHeap(b *testing.B) {
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // the parser logs each model
	dir := b.TempDir()
	proj := filepath.Join(dir, "projects", "-tmp-demo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, plainID+".jsonl"), longTranscript(400), 0o644); err != nil {
		b.Fatal(err)
	}

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()

	var events int
	for i := 0; i < b.N; i++ {
		for s, err := range New(dir).Sessions(time.Time{}) {
			if err != nil {
				b.Fatal(err)
			}
			events = len(s.Events)
			runtime.GC()
			var held runtime.MemStats
			runtime.ReadMemStats(&held)
			b.ReportMetric(float64(int64(held.HeapAlloc)-int64(before.HeapAlloc))/1048576, "MB-retained")
		}
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(int64(after.HeapSys)-int64(before.HeapSys))/1048576, "MB-HeapSys")
	b.ReportMetric(float64(events), "events")
}

// longTranscript is a transcript of n turns, each a tool call with a 4 KB result.
func longTranscript(n int) []byte {
	var sb strings.Builder
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	line := func(v any) {
		raw, _ := json.Marshal(v)
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	user := func(content any) {
		at = at.Add(time.Second)
		line(map[string]any{"type": "user", "cwd": "/tmp/demo", "version": "2.1.300", "timestamp": at,
			"message": map[string]any{"role": "user", "content": content}})
	}
	user("Read every file.")
	result := strings.Repeat("x", 4096)
	for i := range n {
		at = at.Add(time.Second)
		id := fmt.Sprintf("toolu_%06d", i)
		line(map[string]any{"type": "assistant", "cwd": "/tmp/demo", "version": "2.1.300", "timestamp": at,
			"requestId": fmt.Sprintf("req_%06d", i),
			"message": map[string]any{"id": fmt.Sprintf("msg_%06d", i), "model": "claude-sonnet-4-5-20250929",
				"content":     []any{map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"path": fmt.Sprintf("/f%d", i)}}},
				"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}})
		user([]any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": fmt.Sprintf("%d%s", i, result)}})
	}
	return []byte(sb.String())
}
