package forwardproxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// mocactlStream is the shape that exposed the bug: a harness turn whose every
// event is typed by its event: line and carries an id: for resume, plus the
// fields a byte-faithful relay must also keep — a heartbeat, a retry:, CRLF
// terminators, and a final event the upstream never terminated.
const mocactlStream = ": connected\n\n" +
	"event: turn\nid: 1\ndata: {\"type\":\"turn\",\"turnId\":\"t1\"}\n\n" +
	"retry: 3000\n\n" +
	"event: text\r\nid: 2\r\ndata: {\"type\":\"text\",\"delta\":\"Hello\"}\r\n\r\n" +
	"event: done\nid: 3\ndata: {\"type\":\"done\",\"stopReason\":\"stop\"}"

func sseRelayProxy(t *testing.T, plugin pipeline.Plugin, upstream http.Handler) (*http.Client, string) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	pipe, err := pipeline.New([]pipeline.Plugin{plugin})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	return client, up.URL + "/v1/turn"
}

// With a StreamingResponder in the chain the client still receives the
// upstream's bytes unchanged, and the plugin still sees each event's payload.
func TestForwardProxy_Streaming_RelaysBytesUnchanged(t *testing.T) {
	probe := newStreamingProbe(false)
	client, url := sseRelayProxy(t, probe, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mocactlStream)
	}))

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != mocactlStream {
		t.Fatalf("relayed body differs from upstream\n got %q\nwant %q", body, mocactlStream)
	}

	frames, lasts := probe.snapshot()
	want := []string{
		`{"type":"turn","turnId":"t1"}`,
		`{"type":"text","delta":"Hello"}`,
		`{"type":"done","stopReason":"stop"}`,
		"", // the last=true call
	}
	got := framesAsStrings(frames)
	if len(got) != len(want) {
		t.Fatalf("plugin saw %q (lasts %v), want %q", got, lasts, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !lasts[len(lasts)-1] {
		t.Error("final dispatch last=false, want true")
	}
}

// A heartbeat reaches the client while the upstream is still open — the
// relay must not hold a data-less block until the next event.
func TestForwardProxy_Streaming_HeartbeatDeliveredWhileUpstreamOpen(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client, url := sseRelayProxy(t, newStreamingProbe(false), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": ping\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		line, _ := br.ReadString('\n')
		blank, _ := br.ReadString('\n')
		got <- line + blank
	}()
	// The upstream stays open, so the heartbeat arrives only if it is relayed as it comes.
	if s := <-got; s != ": ping\n\n" {
		t.Errorf("first bytes = %q, want %q", s, ": ping\n\n")
	}
}

// rejectSecondFrame rejects the second data frame it sees.
type rejectSecondFrame struct{ seen int }

func (p *rejectSecondFrame) Name() string { return "reject-second-frame" }
func (p *rejectSecondFrame) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (p *rejectSecondFrame) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *rejectSecondFrame) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *rejectSecondFrame) OnResponseFrame(_ context.Context, _ *pipeline.Context, _ []byte, last bool) pipeline.Action {
	if last {
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.seen++
	if p.seen == 2 {
		return pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{Code: "test.reject", Reason: "second frame"}}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// Plugins still run before a block is written, so a rejected block never
// reaches the client.
func TestForwardProxy_Streaming_RejectedBlockNotForwarded(t *testing.T) {
	const first = "event: turn\nid: 1\ndata: {\"type\":\"turn\"}\n\n"
	const second = "event: text\nid: 2\ndata: {\"type\":\"text\"}\n\n"
	client, url := sseRelayProxy(t, &rejectSecondFrame{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first+second)
	}))

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != first {
		t.Fatalf("body = %q, want only the first block %q", body, first)
	}
}
