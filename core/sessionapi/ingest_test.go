package sessionapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

const testToken = "tok-123"

// ingestCaptures is a session.CaptureRecorder that keeps what it is handed, by request id.
type ingestCaptures struct {
	mu  sync.Mutex
	req map[string]*model.RequestCapture
	res map[string]*model.ResponseCapture
}

func (c *ingestCaptures) Record(string, *model.SessionEvent) {}

func (c *ingestCaptures) RecordCapture(_ string, e *model.SessionEvent, rc *model.RequestCapture) {
	if rc == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rc.Response != nil {
		c.res[e.RequestID] = rc.Response
		return
	}
	c.req[e.RequestID] = rc
}

func (c *ingestCaptures) get(id string) (*model.RequestCapture, *model.ResponseCapture) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.req[id], c.res[id]
}

type ingestRig struct {
	srv   *Server
	store *session.Store
	caps  *ingestCaptures
	base  string
}

func newIngestRig(t *testing.T, opts ...Option) *ingestRig {
	t.Helper()
	store := session.New(time.Hour, 0, 0)
	t.Cleanup(store.Close)
	caps := &ingestCaptures{req: map[string]*model.RequestCapture{}, res: map[string]*model.ResponseCapture{}}
	store.AddRecorder(caps)
	opts = append([]Option{WithClearAllowed(true), WithIngest(Ingest{Token: testToken})}, opts...)
	srv := New("", store, opts...)
	ts := httptest.NewServer(srv.Server().Handler)
	t.Cleanup(ts.Close)
	return &ingestRig{srv: srv, store: store, caps: caps, base: ts.URL}
}

// post sends lines as one NDJSON body and returns the status.
func (r *ingestRig) post(t *testing.T, edit func(*http.Request), lines ...[]byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.base+"/v1/ingest", bytes.NewReader(append(bytes.Join(lines, []byte("\n")), '\n')))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (r *ingestRig) stats(t *testing.T) IngestStats {
	t.Helper()
	resp, err := http.Get(r.base + "/v1/ingest/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st IngestStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (r *ingestRig) events(sid string) []model.SessionEvent {
	v := r.store.View(sid)
	if v == nil {
		return nil
	}
	return v.Events
}

// line is one v1 ingest line; fields override the defaults.
func line(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	l := map[string]any{
		"v": 1, "schemaVersion": SchemaVersion, "op": "append",
		"requestId": "r1", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"session": "s1",
		"event": map[string]any{
			"at": "2020-01-01T00:00:00Z", "phase": "request", "direction": "outbound",
			"requestId": "r1", "host": "api.anthropic.com", "httpPath": "/v1/messages",
			"inference": map[string]any{"model": "claude-x"},
		},
	}
	for k, v := range fields {
		if v == nil {
			delete(l, k)
			continue
		}
		l[k] = v
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func responseEvent(extra map[string]any) map[string]any {
	ev := map[string]any{
		"at": "2020-01-01T00:00:00Z", "phase": "response", "direction": "outbound",
		"requestId": "r1", "host": "api.anthropic.com", "httpPath": "/v1/messages",
		"inference": map[string]any{"model": "claude-x"},
	}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

func TestIngest_NotRegisteredWithoutOption(t *testing.T) {
	srv := New("", session.New(time.Hour, 0, 0))
	if srv.ingest != nil {
		t.Fatal("ingest state exists without WithIngest: no erase marks may be kept in direct mode")
	}
	if srv2 := New("", session.New(time.Hour, 0, 0), WithIngest(Ingest{})); srv2.ingest != nil {
		t.Fatal("an empty token must not turn ingestion on")
	}
	ts := httptest.NewServer(srv.Server().Handler)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/ingest", "application/x-ndjson", strings.NewReader("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /v1/ingest = %d, want 404", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/v1/ingest/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/ingest/stats = %d, want 404", resp.StatusCode)
	}
}

func TestIngest_Unauthorized(t *testing.T) {
	r := newIngestRig(t)
	for name, edit := range map[string]func(*http.Request){
		"none":  func(q *http.Request) { q.Header.Del("Authorization") },
		"wrong": func(q *http.Request) { q.Header.Set("Authorization", "Bearer nope") },
	} {
		if got := r.post(t, edit, line(t, nil)); got != http.StatusUnauthorized {
			t.Errorf("%s token: status %d, want 401", name, got)
		}
	}
	if len(r.events("s1")) != 0 {
		t.Error("an unauthorized line was stored")
	}
}

func TestIngest_RefusesOriginAndForeignHost(t *testing.T) {
	r := newIngestRig(t)
	for name, edit := range map[string]func(*http.Request){
		"origin": func(q *http.Request) { q.Header.Set("Origin", "https://evil.example") },
		"host":   func(q *http.Request) { q.Host = "evil.example" },
	} {
		if got := r.post(t, edit, line(t, nil)); got != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
	if len(r.events("s1")) != 0 {
		t.Error("a refused line was stored")
	}
}

func TestIngest_RefusalsCountedConnectionStays(t *testing.T) {
	r := newIngestRig(t)
	big := bytes.Repeat([]byte("x"), ingestLineCap+1)
	got := r.post(t, nil,
		[]byte("{not json"),
		line(t, map[string]any{"v": 2}),
		line(t, map[string]any{"schemaVersion": 2}),
		line(t, map[string]any{"op": "pair"}),
		big,
		line(t, nil),
	)
	if got != http.StatusNoContent {
		t.Fatalf("status %d, want 204", got)
	}
	if n := len(r.events("s1")); n != 1 {
		t.Fatalf("stored %d rows, want the 1 good one", n)
	}
	st := r.stats(t)
	if st.Lines != 1 {
		t.Errorf("ingest_lines_total = %d, want 1", st.Lines)
	}
	for _, reason := range []string{"bad_json", "version", "schema", "op", "line_cap"} {
		if st.Refused[reason] != 1 {
			t.Errorf("ingest_refused_total[%s] = %d, want 1", reason, st.Refused[reason])
		}
	}
	if st.Refused["erased"] != 0 {
		t.Errorf("erased = %d, want 0", st.Refused["erased"])
	}
}

func TestIngest_StoreAssignsSeqAndAt(t *testing.T) {
	r := newIngestRig(t)
	ev := map[string]any{"seq": 99, "sessionId": "forged", "at": "2001-01-01T00:00:00Z", "phase": "request",
		"requestId": "r1", "inference": map[string]any{"model": "m"}}
	r.post(t, nil, line(t, map[string]any{"event": ev}))
	evs := r.events("s1")
	if len(evs) != 1 {
		t.Fatalf("got %d rows", len(evs))
	}
	if evs[0].Seq != 1 {
		t.Errorf("seq = %d, want 1", evs[0].Seq)
	}
	if evs[0].SessionID != "s1" {
		t.Errorf("sessionId = %q, want s1", evs[0].SessionID)
	}
	if d := time.Since(evs[0].At); d < 0 || d > time.Second {
		t.Errorf("at = %v, want within 1s of now", evs[0].At)
	}
}

func TestIngest_ProducerCostIsReplaced(t *testing.T) {
	r := newIngestRig(t)
	forged := map[string]any{"cost_usd": 999, "source": "gateway_header", "settled": true}
	ev := responseEvent(map[string]any{"plugins": map[string]any{event.Key: forged, event.PluginName: forged}})
	r.post(t, nil, line(t, map[string]any{"event": ev,
		"cost": map[string]any{"headers": map[string]string{"X-Litellm-Response-Cost": "0.01", "Content-Type": "application/json"}}}))
	evs := r.events("s1")
	if len(evs) != 1 {
		t.Fatalf("got %d rows", len(evs))
	}
	for _, key := range []string{event.Key, event.PluginName} {
		var rec event.Event
		if err := json.Unmarshal(evs[0].Plugins[key], &rec); err != nil {
			t.Fatalf("plugins[%s]: %v (%s)", key, err, evs[0].Plugins[key])
		}
		if rec.CostUSD != 0.01 {
			t.Errorf("plugins[%s].cost_usd = %v, want the settled 0.01", key, rec.CostUSD)
		}
	}
}

func TestIngest_CaptureRedactedAgain(t *testing.T) {
	r := newIngestRig(t)
	r.post(t, nil,
		line(t, map[string]any{"capture": map[string]any{
			"original": []byte(`{"a":1}`), "forwarded": nil,
			"header": map[string][]string{"Authorization": {"Bearer sk"}, "Accept": {"*/*"}},
			"query":  "beta=true&api_key=sk",
		}}),
		line(t, map[string]any{"event": responseEvent(nil), "capture": map[string]any{
			"response": map[string]any{"status": 200, "header": map[string][]string{"Set-Cookie": {"c"}, "Content-Type": {"application/json"}},
				"body": []byte("{}"), "ended": true},
		}}),
	)
	req, res := r.caps.get("r1")
	if req == nil || res == nil {
		t.Fatalf("captures: request %v, response %v", req, res)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("Accept") != "*/*" {
		t.Errorf("request header = %v", req.Header)
	}
	if req.Query != "beta=true" {
		t.Errorf("query = %q, want beta=true", req.Query)
	}
	if string(req.Original) != `{"a":1}` || req.Forwarded != nil {
		t.Errorf("original %q forwarded %q", req.Original, req.Forwarded)
	}
	if res.Header.Get("Set-Cookie") != "" || res.Header.Get("Content-Type") == "" {
		t.Errorf("response header = %v", res.Header)
	}
	if res.Status != 200 || string(res.Body) != "{}" || !res.Ended {
		t.Errorf("response = %+v", res)
	}
}

func TestIngest_CaptureOnlyForInference(t *testing.T) {
	r := newIngestRig(t)
	ev := map[string]any{"phase": "request", "requestId": "r1", "host": "oauth.example"}
	r.post(t, nil, line(t, map[string]any{"event": ev, "capture": map[string]any{"original": []byte("secret=1")}}))
	if len(r.events("s1")) != 1 {
		t.Fatal("the row was not stored")
	}
	if req, _ := r.caps.get("r1"); req != nil {
		t.Error("a capture was kept for a row without inference")
	}
}

func TestIngest_DroppedCaptureKeepsRow(t *testing.T) {
	r := newIngestRig(t)
	r.post(t, nil, line(t, map[string]any{"capture": map[string]any{"dropped": true}}))
	if len(r.events("s1")) != 1 {
		t.Fatal("the row was not stored")
	}
	if req, _ := r.caps.get("r1"); req != nil {
		t.Error("a dropped capture was stored")
	}
	if st := r.stats(t); st.CapturesDropped != 1 {
		t.Errorf("ingest_captures_dropped_total = %d, want 1", st.CapturesDropped)
	}
}

func TestIngest_NoResponseCaptureWithoutRequestCapture(t *testing.T) {
	r := newIngestRig(t)
	r.post(t, nil,
		line(t, nil),
		line(t, map[string]any{"event": responseEvent(nil), "capture": map[string]any{
			"response": map[string]any{"status": 200, "body": []byte("{}")}}}),
	)
	if len(r.events("s1")) != 2 {
		t.Fatal("the rows were not stored")
	}
	if _, res := r.caps.get("r1"); res != nil {
		t.Error("a response capture was kept with no request capture")
	}
}

func eraseLines(t *testing.T, r *ingestRig, erase func()) {
	t.Helper()
	before := time.Now().UTC()
	r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano)}))
	if len(r.events("s1")) != 1 {
		t.Fatal("the first row was not stored")
	}
	erase()
	if r.store.View("s1") != nil {
		t.Fatal("the erase did not remove the session")
	}
	r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano), "requestId": "r2"}))
	if r.store.View("s1") != nil {
		t.Fatal("a line queued before the erase brought the session back")
	}
	if st := r.stats(t); st.Refused["erased"] != 1 {
		t.Errorf("ingest_refused_total[erased] = %d, want 1", st.Refused["erased"])
	}
	r.post(t, nil, line(t, map[string]any{"startedAt": time.Now().UTC().Format(time.RFC3339Nano), "requestId": "r3"}))
	evs := r.events("s1")
	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Fatalf("a request after the erase: %d rows, want a new session with 1", len(evs))
	}
}

func del(t *testing.T, url string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE %s = %d", url, resp.StatusCode)
	}
}

func TestIngest_EraseDropsOlderLines(t *testing.T) {
	r := newIngestRig(t)
	eraseLines(t, r, func() { del(t, r.base+"/v1/sessions/s1") })
}

func TestIngest_ClearDropsOlderLines(t *testing.T) {
	r := newIngestRig(t)
	eraseLines(t, r, func() { del(t, r.base+"/v1/sessions") })
}

func TestIngest_EraseMarkExpires(t *testing.T) {
	for _, path := range []string{"/v1/sessions/s1", "/v1/sessions"} {
		t.Run(path, func(t *testing.T) {
			r := newIngestRig(t)
			before := time.Now().UTC()
			r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano)}))
			del(t, r.base+path)
			r.srv.ingest.marks.setClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
			r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano), "requestId": "r2"}))
			if len(r.events("s1")) != 1 {
				t.Fatal("a line older than an expired erase mark was not kept")
			}
		})
	}
}

func TestIngest_EmptySessionIsFiled(t *testing.T) {
	r := newIngestRig(t)
	r.store.Append("active", model.SessionEvent{})
	r.post(t, nil, line(t, map[string]any{"session": ""}))
	if n := len(r.events("active")); n != 2 {
		t.Fatalf("active session has %d rows, want 2", n)
	}
}

func TestIngest_ShutdownEndsStream(t *testing.T) {
	store := session.New(time.Hour, 0, 0)
	defer store.Close()
	srv := New("", store, WithIngest(Ingest{Token: testToken}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Server().Serve(ln) }()

	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/v1/ingest", ln.Addr()), pr)
	req.Header.Set("Authorization", "Bearer "+testToken)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	if _, err := pw.Write(append(line(t, nil), '\n')); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for store.View("s1") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the line never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Server().Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Shutdown took %v, want under 1s", d)
	}
}

func TestIngest_ResponseGetsRequestFields(t *testing.T) {
	r := newIngestRig(t)
	reqInf := map[string]any{"model": "claude-x",
		"messages":         []map[string]any{{"role": "user", "content": "hi"}},
		"tools":            []map[string]any{{"name": "Read"}},
		"toolResults":      []map[string]any{{"toolUseId": "t1", "content": "ok"}},
		"historyToolCalls": []map[string]any{{"id": "t1", "name": "Read"}},
		"historyThinking":  map[string]any{"blocks": 1}}
	reqEv := map[string]any{"phase": "request", "requestId": "r1", "inference": reqInf}
	r.post(t, nil,
		line(t, map[string]any{"event": reqEv}),
		// A response row of format v1: the request-only fields are left out.
		line(t, map[string]any{"event": responseEvent(map[string]any{"inference": map[string]any{"model": "claude-x", "outputTokens": 5}})}),
		// A response row that carries its own messages keeps them.
		line(t, map[string]any{"requestId": "r2", "event": map[string]any{"phase": "request", "requestId": "r2", "inference": reqInf}}),
		line(t, map[string]any{"requestId": "r2", "event": responseEvent(map[string]any{"requestId": "r2", "inference": map[string]any{"model": "claude-x",
			"messages": []map[string]any{{"role": "user", "content": "own"}}}})}),
		// A response with no request row stays as it is.
		line(t, map[string]any{"requestId": "r3", "event": responseEvent(map[string]any{"requestId": "r3"})}),
	)
	evs := r.events("s1")
	if len(evs) != 5 {
		t.Fatalf("got %d rows, want 5", len(evs))
	}
	req, res := evs[0].Inference, evs[1].Inference
	if res.OutputTokens != 5 {
		t.Errorf("the response's own field was lost: outputTokens = %d", res.OutputTokens)
	}
	for name, eq := range map[string]bool{
		"messages":         len(res.Messages) == 1 && res.Messages[0].Content == req.Messages[0].Content,
		"tools":            len(res.Tools) == 1 && res.Tools[0].Name == "Read",
		"toolResults":      len(res.ToolResults) == 1,
		"historyToolCalls": len(res.HistoryToolCalls) == 1,
		"historyThinking":  res.HistoryThinking != nil && res.HistoryThinking.Blocks == 1,
	} {
		if !eq {
			t.Errorf("response %s was not restored from the request row", name)
		}
	}
	if m := evs[3].Inference.Messages; len(m) != 1 || m[0].Content != "own" {
		t.Errorf("a response's own messages were replaced: %+v", m)
	}
	if m := evs[4].Inference.Messages; m != nil {
		t.Errorf("a response with no request row got messages: %+v", m)
	}
	r.srv.ingest.reqCapsMu.Lock()
	n := len(r.srv.ingest.reqInf)
	r.srv.ingest.reqCapsMu.Unlock()
	if n != 0 {
		t.Errorf("%d held requests remain after their responses", n)
	}
}

func TestIngest_EraseForgetsHeldRequests(t *testing.T) {
	held := func(r *ingestRig) int {
		r.srv.ingest.reqCapsMu.Lock()
		defer r.srv.ingest.reqCapsMu.Unlock()
		return len(r.srv.ingest.reqInf)
	}
	for name, erase := range map[string]func(r *ingestRig){
		"delete": func(r *ingestRig) { del(t, r.base+"/v1/sessions/s1") },
		"clear":  func(r *ingestRig) { del(t, r.base+"/v1/sessions") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newIngestRig(t)
			r.post(t, nil, line(t, nil), line(t, map[string]any{"requestId": "r2", "session": "s2"}))
			if held(r) != 2 {
				t.Fatalf("held %d requests, want 2", held(r))
			}
			erase(r)
			want := 1 // s2 stays
			if name == "clear" {
				want = 0
			}
			if got := held(r); got != want {
				t.Errorf("after the erase, %d held requests, want %d", got, want)
			}
		})
	}
}

func TestIngest_ParallelDecodeKeepsOrder(t *testing.T) {
	r := newIngestRig(t)
	var lines [][]byte
	for i := range 200 {
		id := fmt.Sprintf("r%03d", i)
		// Uneven sizes, so a later line can finish decoding first.
		pad := strings.Repeat("x", (i%7)*20000)
		lines = append(lines, line(t, map[string]any{"requestId": id, "event": map[string]any{"phase": "request",
			"requestId": id, "inference": map[string]any{"model": "m", "messages": []map[string]any{{"role": "user", "content": pad}}}}}))
	}
	lines = append(lines, []byte("{not json"))
	if got := r.post(t, nil, lines...); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	evs := r.events("s1")
	if len(evs) != 200 {
		t.Fatalf("got %d rows, want 200", len(evs))
	}
	for i, ev := range evs {
		if want := fmt.Sprintf("r%03d", i); ev.RequestID != want {
			t.Fatalf("row %d is %s, want %s: lines were filed out of order", i, ev.RequestID, want)
		}
	}
	if st := r.stats(t); st.Lines != 200 || st.Refused["bad_json"] != 1 {
		t.Errorf("stats = %+v", st)
	}
}
