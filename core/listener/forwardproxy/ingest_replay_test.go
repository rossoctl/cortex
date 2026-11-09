package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// The replay test pins praxis mode against direct mode (spec Section 5, [DR 4]). A direct-mode
// forward proxy records every claude-code fixture. Its rows become v1 ingest lines, as
// cortex-gateway writes them, in testdata/ingest/replay.ndjson. Ingesting that file must give
// the same session, summary and usage totals. praxis/filters/tests/row_parity.rs reads the
// same file as the golden of the Rust row builder, so one file pins both sides.

var updateReplay = flag.Bool("update", false, "rewrite core/sessionapi/testdata/ingest/replay.ndjson from a live run")

const (
	replayFile     = "../../sessionapi/testdata/ingest/replay.ndjson"
	replayFixtures = "../../plugins/inferenceparser/testdata/claude-code"
	replaySession  = "00000000-0000-4000-8000-000000000000"
	replayToken    = "replay-token"
	// replayCost is the gateway cost header that every second case carries.
	replayCost = "0.0123"
)

// replayCase is one fixture: the request the agent sends and the response the origin gives.
type replayCase struct {
	name       string
	method     string
	host       string
	target     string
	reqHeader  http.Header
	reqBody    []byte
	status     int
	respHeader http.Header
	respBody   []byte
}

// replayCases loads every claude-code fixture with the parsefixture rules, in name order.
func replayCases(t *testing.T) []replayCase {
	t.Helper()
	heads, err := filepath.Glob(filepath.Join(replayFixtures, "*", "request.headers"))
	if err != nil || len(heads) == 0 {
		t.Fatalf("no fixtures under %s: %v", replayFixtures, err)
	}
	endHead := func(b []byte) []byte { return append(bytes.TrimRight(b, "\n"), "\n\n"...) }
	readOpt := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	var out []replayCase
	for i, head := range heads {
		dir := filepath.Dir(head)
		raw, _ := os.ReadFile(head)
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(endHead(raw))))
		if err != nil {
			t.Fatalf("%s: %v", head, err)
		}
		c := replayCase{name: filepath.Base(dir), method: req.Method, host: req.Host, target: req.RequestURI,
			reqHeader: req.Header, reqBody: readOpt(filepath.Join(dir, "request.json")), status: http.StatusOK}
		if c.name == "error-502-html" {
			c.status = http.StatusBadGateway
		}
		c.respHeader = http.Header{"Content-Type": {"application/json"}}
		c.respBody = readOpt(filepath.Join(dir, "response.json"))
		for file, ct := range map[string]string{"response.sse": "text/event-stream", "response.html": "text/html"} {
			if b, err := os.ReadFile(filepath.Join(dir, file)); err == nil {
				c.respHeader, c.respBody = http.Header{"Content-Type": {ct}}, b
			}
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "response.headers")); err == nil {
			h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(endHead(raw)))).ReadMIMEHeader()
			if err != nil {
				t.Fatalf("%s/response.headers: %v", dir, err)
			}
			c.respHeader = http.Header(h)
		}
		if i%2 == 0 {
			c.respHeader.Set("X-Litellm-Response-Cost", replayCost)
		}
		c.respHeader.Set("Content-Length", strconv.Itoa(len(c.respBody)))
		out = append(out, c)
	}
	return out
}

func replayRates(t *testing.T) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range map[pricing.Tier]float64{
		pricing.TierInput: 7e-6, pricing.TierCacheWrite: 11e-6, pricing.TierCacheRead: 3e-6, pricing.TierOutput: 23e-6,
	} {
		r.Base[tier], r.Set[tier] = perToken, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// replayRig is a store with a usage aggregator, served by a session API.
type replayRig struct {
	store *session.Store
	base  string
}

func newReplayRig(t *testing.T, opts ...sessionapi.Option) *replayRig {
	t.Helper()
	store := session.New(time.Hour, 0, 0)
	t.Cleanup(store.Close)
	agg := usage.New()
	store.AddRecorder(agg)
	srv := sessionapi.New("", store, append([]sessionapi.Option{sessionapi.WithUsage(agg)}, opts...)...)
	ts := httptest.NewServer(srv.Server().Handler)
	t.Cleanup(ts.Close)
	return &replayRig{store: store, base: ts.URL}
}

func (r *replayRig) getJSON(t *testing.T, path string) any {
	t.Helper()
	resp, err := http.Get(r.base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return v
}

// liveRun sends each case through a direct-mode forward proxy, one at a time, and returns the
// rig that holds its rows and the captures it took.
func liveRun(t *testing.T, cases []replayCase, rates pricing.Resolver) (*replayRig, *captures) {
	t.Helper()
	var mu sync.Mutex
	var cur replayCase
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		c := cur
		mu.Unlock()
		for k, vs := range c.respHeader {
			w.Header()[k] = vs
		}
		w.Header()["Date"] = nil // a fixed head, so the captures are the same in every run
		w.WriteHeader(c.status)
		w.Write(c.respBody)
	}))
	t.Cleanup(origin.Close)

	rig := newReplayRig(t)
	rec := &captures{}
	rig.store.AddRecorder(rec)
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(rates)
	p, err := pipeline.New([]pipeline.Plugin{parser})
	if err != nil {
		t.Fatal(err)
	}
	// Every host dials the origin, so rows carry the fixture's host, not a test port.
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: rig.store,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           &http.Client{Transport: &http.Transport{DialContext: dial}}}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL)), DisableCompression: true}}

	for i, c := range cases {
		mu.Lock()
		cur = c
		mu.Unlock()
		req, _ := http.NewRequest(c.method, "http://"+c.host+c.target, bytes.NewReader(c.reqBody))
		for k, vs := range c.reqHeader {
			req.Header[k] = vs
		}
		req.Header.Set(session.ClaudeCodeSessionHeader, replaySession)
		if req.Header.Get("User-Agent") == "" {
			req.Header["User-Agent"] = []string{""} // no Go default: the Rust side sees only the fixture's headers
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// The response row lands after the last byte reaches the client: wait for it, so the
		// rows stay in case order.
		deadline := time.Now().Add(5 * time.Second)
		for {
			v := rig.store.View(replaySession)
			if v != nil && len(v.Events) >= 2*(i+1) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no response row", c.name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return rig, rec
}

// The wire form of a capture (ingestCapture in core/sessionapi/ingest.go), with the keys
// cortex-gateway writes: a request capture has no forwarded copy, a response capture only
// its response.
type replayCapture struct {
	Original  []byte          `json:"original,omitempty"`
	Forwarded []byte          `json:"forwarded,omitempty"`
	Header    http.Header     `json:"header,omitempty"`
	Query     string          `json:"query,omitempty"`
	Response  *replayResponse `json:"response,omitempty"`
}

type replayResponse struct {
	Status     int         `json:"status"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"body"`
	Decoded    bool        `json:"decoded"`
	Truncated  bool        `json:"truncated"`
	Ended      bool        `json:"ended"`
	Incomplete bool        `json:"incomplete"`
}

type replayLine struct {
	V             int                   `json:"v"`
	SchemaVersion int                   `json:"schemaVersion"`
	Op            string                `json:"op"`
	RequestID     string                `json:"requestId"`
	StartedAt     string                `json:"startedAt"`
	Session       string                `json:"session"`
	Event         pipeline.SessionEvent `json:"event"`
	Capture       *replayCapture        `json:"capture,omitempty"`
	Cost          map[string]any        `json:"cost,omitempty"`
}

// replayLines converts the live run's rows and captures to v1 lines, as a producer sends them.
func replayLines(t *testing.T, rig *replayRig, rec *captures, cases []replayCase) []byte {
	t.Helper()
	v := rig.store.View(replaySession)
	if v == nil || len(v.Events) != 2*len(cases) {
		t.Fatalf("live run has %v rows, want %d", v, 2*len(cases))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var buf bytes.Buffer
	for i, ev := range v.Events {
		l := replayLine{V: 1, SchemaVersion: sessionapi.SchemaVersion, Op: "append", RequestID: ev.RequestID,
			StartedAt: ev.At.UTC().Format(time.RFC3339Nano), Session: replaySession}
		if rc := rec.got[ev.RequestID]; ev.Phase == pipeline.SessionRequest && rc != nil {
			l.Capture = &replayCapture{Original: rc.Original, Forwarded: rc.Forwarded, Header: rc.Header, Query: rc.Query}
		}
		if ev.Phase == pipeline.SessionResponse {
			if rc := rec.res[ev.RequestID]; rc != nil {
				l.Capture = &replayCapture{Response: &replayResponse{Status: rc.Status, Header: rc.Header, Body: rc.Body,
					Decoded: rc.Decoded, Truncated: rc.Truncated, Ended: rc.Ended, Incomplete: rc.Incomplete}}
			}
			hdr := map[string]string{}
			for _, k := range []string{"X-Litellm-Response-Cost", "X-Litellm-Model-Name", "Content-Type"} {
				if s := cases[i/2].respHeader.Get(k); s != "" {
					hdr[k] = s
				}
			}
			l.Cost = map[string]any{"headers": hdr}
		}
		if ev.Phase == pipeline.SessionResponse && ev.Inference != nil {
			// Line format v1: a response row leaves out the request-only fields, and Cortex
			// puts them back from the request row. The live session still has them.
			inf := *ev.Inference
			inf.Messages, inf.Tools, inf.ToolResults, inf.HistoryToolCalls, inf.HistoryThinking = nil, nil, nil, nil, nil
			ev.Inference = &inf
		}
		ev.Seq, ev.SessionID, ev.At = 0, "", ev.At.UTC()
		l.Event = ev
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	return buf.Bytes()
}

// strip removes the keys from each object in v, at any depth.
func strip(v any, keys ...string) any {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range keys {
			delete(v, k)
		}
		for _, c := range v {
			strip(c, keys...)
		}
	case []any:
		for _, c := range v {
			strip(c, keys...)
		}
	}
	return v
}

func sameJSON(t *testing.T, what string, live, replayed any) {
	t.Helper()
	if !reflect.DeepEqual(live, replayed) {
		l, _ := json.MarshalIndent(live, "", "  ")
		r, _ := json.MarshalIndent(replayed, "", "  ")
		t.Errorf("%s differs\nlive:\n%s\nreplayed:\n%s", what, l, r)
	}
}

func TestIngestReplay_MatchesLiveRun(t *testing.T) {
	cases := replayCases(t)
	rates := replayRates(t)
	live, rec := liveRun(t, cases, rates)

	lines := replayLines(t, live, rec, cases)
	if *updateReplay {
		if err := os.MkdirAll(filepath.Dir(replayFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replayFile, lines, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.ReadFile(replayFile)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}

	replay := newReplayRig(t, sessionapi.WithIngest(sessionapi.Ingest{Token: replayToken, Rates: rates}))
	req, _ := http.NewRequest(http.MethodPost, replay.base+"/v1/ingest", bytes.NewReader(file))
	req.Header.Set("Authorization", "Bearer "+replayToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ingest answered %d", resp.StatusCode)
	}
	stats := replay.getJSON(t, "/v1/ingest/stats").(map[string]any)
	if got := stats["ingest_lines_total"]; got != float64(2*len(cases)) {
		t.Fatalf("ingested %v lines, want %d: %v", got, 2*len(cases), stats)
	}

	// The file comes from an earlier run: its request ids and timings are that run's.
	volatile := []string{"at", "requestId", "durationMs", "ttfbMs"}
	path := "/v1/sessions/" + replaySession
	sameJSON(t, path, strip(live.getJSON(t, path), volatile...), strip(replay.getJSON(t, path), volatile...))
	summaryTimes := []string{"createdAt", "updatedAt", "lastRequestAt", "at"}
	sameJSON(t, "/v1/sessions", strip(live.getJSON(t, "/v1/sessions"), summaryTimes...),
		strip(replay.getJSON(t, "/v1/sessions"), summaryTimes...))
	totals := func(r *replayRig) any { return r.getJSON(t, "/v1/usage").(map[string]any)["totals"] }
	sameJSON(t, "/v1/usage totals", totals(live), totals(replay))
	if t.Failed() {
		t.Log("when direct mode changed on purpose, run with -update")
	}
}
