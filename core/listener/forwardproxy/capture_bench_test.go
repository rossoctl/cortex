package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// BenchmarkCapture_RequestPath is one 600 KB request through the proxy to a local origin, with
// the store archiving to disk. Run with -bench 'RequestPath/(off|on)' to compare: capture must
// add nothing measurable before the request goes out, because the split, the hashes and the
// disk write all run on the archive's writer goroutine.
func BenchmarkCapture_RequestPath(b *testing.B) {
	body := `{"model":"m","system":"` + strings.Repeat("a system prompt ", 40000) + `","messages":[{"role":"user","content":"hi"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	for _, mode := range []string{"off", "on"} {
		b.Run(mode, func(b *testing.B) {
			p, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
			if err != nil {
				b.Fatal(err)
			}
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			arch, err := archive.Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer arch.Close()
			if mode == "on" {
				store.AddRecorder(arch)
			} else {
				store.AddRecorder(eventsOnly{arch})
			}
			srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
			proxy := httptest.NewServer(srv.Handler())
			defer proxy.Close()
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				resp, err := client.Post(backend.URL+"/v1/messages", "application/json", strings.NewReader(body))
				if err != nil {
					b.Fatal(err)
				}
				resp.Body.Close()
			}
			b.ReportMetric(float64(arch.Stats().DroppedCaptures), "dropped-captures")
		})
	}
}

// eventsOnly hides the archive's RecordCapture, so the store hands it events alone.
type eventsOnly struct{ a *archive.Archive }

func (e eventsOnly) Record(id string, ev *pipeline.SessionEvent) { e.a.Record(id, ev) }
