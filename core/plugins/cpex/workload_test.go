package cpex

import (
	"context"
	"errors"
	"testing"
)

type fakeJWT struct {
	tok   string
	err   error
	calls int
}

func (f *fakeJWT) FetchToken(context.Context) (string, error) {
	f.calls++
	return f.tok, f.err
}

// The pod's SVID replaces a caller's own copy of the header, and only
// when the request is one identity is injected on.
func TestWorkloadSourceApply(t *testing.T) {
	src := &fakeJWT{tok: "svid"}
	w := newWorkloadSource(workloadConfig{Audience: "aud", Header: "X-Workload-Token"}, src)
	caller := map[string]string{"x-workload-token": "stolen", "x-user-token": "u"}

	got := w.apply(context.Background(), caller, true)
	if got["x-workload-token"] != "svid" || got["x-user-token"] != "u" {
		t.Fatalf("inject: %v", got)
	}
	got = w.apply(context.Background(), map[string]string{"x-workload-token": "stolen"}, false)
	if _, ok := got["x-workload-token"]; ok || src.calls != 1 {
		t.Fatalf("no inject: %v, fetches %d", got, src.calls)
	}
	if got := w.apply(context.Background(), nil, true); got["x-workload-token"] != "svid" {
		t.Fatalf("nil headers: %v", got)
	}
}

// Without a usable SVID nothing is injected, so a route that needs one
// denies; the caller's copy is still dropped.
func TestWorkloadSourceUnavailable(t *testing.T) {
	for name, w := range map[string]*workloadSource{
		"no provider": newWorkloadSource(workloadConfig{Audience: "aud", Header: defaultWorkloadHeader}, nil),
		"fetch fails": newWorkloadSource(workloadConfig{Audience: "aud", Header: defaultWorkloadHeader}, &fakeJWT{err: errors.New("agent down")}),
	} {
		got := w.apply(context.Background(), map[string]string{"x-workload-token": "stolen"}, true)
		if _, ok := got["x-workload-token"]; ok {
			t.Errorf("%s: header present: %v", name, got)
		}
	}
	var off *workloadSource
	if got := off.apply(context.Background(), map[string]string{"x-workload-token": "t"}, true); got["x-workload-token"] != "t" {
		t.Errorf("off: %v", got)
	}
}

func TestConfigure_Workload(t *testing.T) {
	var seen ManagerOptions
	p := NewCPEX()
	p.newManager = func(opts ManagerOptions) (Manager, error) {
		seen = opts
		return &FakeManager{}, nil
	}
	p.testWorkloadJWT = &fakeJWT{tok: "svid"}
	if err := p.Configure([]byte(`{"workload":{"audience":"http://idp/realms/r"}}`)); err != nil {
		t.Fatal(err)
	}
	if seen.Workload == nil || seen.Workload.header != defaultWorkloadHeader || seen.Workload.src == nil {
		t.Fatalf("options = %+v", seen.Workload)
	}
	if err := p.Configure([]byte(`{}`)); err != nil || seen.Workload != nil {
		t.Fatalf("off by default: err = %v, workload = %+v", err, seen.Workload)
	}
	for _, raw := range []string{
		`{"workload":{"audience":"a","header":"Authorization"}}`,
		`{"workload":{"audience":"a","header":"bad header"}}`,
		`{"workload":{"audience":"a"},"principal":{"token_file":"/x","header":"X-Workload-Token"}}`,
	} {
		if err := setupFake(&FakeManager{}).Configure([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
