package sessionapi

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// fakeJoins is a HopJoins that records its calls.
type fakeJoins struct {
	mu       sync.Mutex
	appendOK bool
	cut      *model.EventError
	status   int
	appended []string
	released []string
	taken    []string
}

func (f *fakeJoins) AppendRequest(id, _ string, _ model.SessionEvent, _ *model.RequestCapture) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, id)
	return f.appendOK
}

func (f *fakeJoins) Release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
}

func (f *fakeJoins) TakeCut(id string) (*model.EventError, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken = append(f.taken, id)
	return f.cut, f.status
}

func newJoinsRig(t *testing.T, j HopJoins) *ingestRig {
	t.Helper()
	return newIngestRig(t, WithIngest(Ingest{Token: testToken, Joins: j}))
}

func TestIngestJoins_ErasedLineReleasesJoin(t *testing.T) {
	j := &fakeJoins{}
	r := newJoinsRig(t, j)
	before := time.Now().UTC()
	r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano)}))
	del(t, r.base+"/v1/sessions/s1")
	j.appended = nil
	r.post(t, nil, line(t, map[string]any{"startedAt": before.Format(time.RFC3339Nano), "requestId": "r2"}))
	if len(j.released) != 1 || j.released[0] != "r2" {
		t.Errorf("released = %v, want [r2]", j.released)
	}
	if len(j.appended) != 0 {
		t.Errorf("appended = %v, want none for an erased line", j.appended)
	}
	if r.store.View("s1") != nil {
		t.Error("an erased line was appended")
	}
	if st := r.stats(t); st.Refused["erased"] != 1 {
		t.Errorf("ingest_refused_total[erased] = %d, want 1", st.Refused["erased"])
	}
}

func TestIngestJoins_RequestRowGoesThroughJoin(t *testing.T) {
	j := &fakeJoins{appendOK: true}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, nil))
	if len(j.appended) != 1 || j.appended[0] != "r1" {
		t.Fatalf("appended = %v, want [r1]", j.appended)
	}
	if r.store.View("s1") != nil {
		t.Error("ingestion appended a row that the join wrote")
	}
	if st := r.stats(t); st.Lines != 1 {
		t.Errorf("ingest_lines_total = %d, want 1", st.Lines)
	}
}

func TestIngestJoins_RequestRowFallsBack(t *testing.T) {
	j := &fakeJoins{}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, nil))
	if len(j.appended) != 1 {
		t.Fatalf("appended = %v, want one call", j.appended)
	}
	if evs := r.events("s1"); len(evs) != 1 || evs[0].Phase != model.SessionRequest {
		t.Fatalf("store holds %d rows, want the request row", len(evs))
	}
}

func TestIngestJoins_ResponseRowNotJoined(t *testing.T) {
	j := &fakeJoins{appendOK: true}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, map[string]any{"event": responseEvent(nil)}))
	if len(j.appended) != 0 {
		t.Errorf("appended = %v, want none for a response row", j.appended)
	}
	if evs := r.events("s1"); len(evs) != 1 {
		t.Fatalf("store holds %d rows, want the response row", len(evs))
	}
}

func TestIngestJoins_CutReplacesClientCanceled(t *testing.T) {
	j := &fakeJoins{cut: &model.EventError{Kind: "proxy_error", Message: "stream idle"}, status: http.StatusOK}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, map[string]any{"event": responseEvent(map[string]any{
		"statusCode": 499,
		"error":      map[string]any{"kind": "client_canceled", "code": "c1", "message": "client went away"}})}))
	evs := r.events("s1")
	if len(evs) != 1 || evs[0].Error == nil {
		t.Fatalf("store holds %d rows, want one response row with an error", len(evs))
	}
	if e := evs[0].Error; e.Kind != "proxy_error" || e.Message != "stream idle" || e.Code != "c1" {
		t.Errorf("error = %+v, want kind proxy_error, message %q, code c1", *e, "stream idle")
	}
	// The row keeps the status Cortex sent, as in direct mode, not the gateway's 499.
	if evs[0].StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", evs[0].StatusCode, http.StatusOK)
	}
	if len(j.taken) != 1 || j.taken[0] != "r1" {
		t.Errorf("taken = %v, want [r1]", j.taken)
	}
}

// A cut with no known status (0) keeps the gateway's status.
func TestIngestJoins_CutWithNoStatusKeepsGatewayStatus(t *testing.T) {
	j := &fakeJoins{cut: &model.EventError{Kind: "proxy_error", Message: "stream idle"}}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, map[string]any{"event": responseEvent(map[string]any{"statusCode": 499})}))
	evs := r.events("s1")
	if len(evs) != 1 || evs[0].StatusCode != 499 {
		t.Fatalf("rows = %+v, want one response row with status 499", evs)
	}
}

func TestIngestJoins_CutOnRowWithNoError(t *testing.T) {
	j := &fakeJoins{cut: &model.EventError{Kind: "proxy_error", Message: "stream idle"}}
	r := newJoinsRig(t, j)
	r.post(t, nil, line(t, map[string]any{"event": responseEvent(nil)}))
	evs := r.events("s1")
	if len(evs) != 1 || evs[0].Error == nil || evs[0].Error.Kind != "proxy_error" {
		t.Fatalf("rows = %+v, want one response row with kind proxy_error", evs)
	}
}

func TestIngestJoins_NilJoinsUnchanged(t *testing.T) {
	r := newIngestRig(t)
	r.post(t, nil, line(t, nil), line(t, map[string]any{"event": responseEvent(map[string]any{
		"error": map[string]any{"kind": "client_canceled"}})}))
	evs := r.events("s1")
	if len(evs) != 2 {
		t.Fatalf("store holds %d rows, want 2", len(evs))
	}
	if evs[1].Error == nil || evs[1].Error.Kind != "client_canceled" {
		t.Errorf("response error = %+v, want client_canceled", evs[1].Error)
	}
}
