package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/rossoctl/accountant"
)

// lateAccountant is a RequestAccountant with the LateOpsAccountant capability. It records the
// spends RequestEnd sees and the ops its late handler gets.
type lateAccountant struct {
	mu       sync.Mutex
	atEnd    int
	late     []accountant.Operation
	keepNone bool
}

func (a *lateAccountant) RequestStart(*Context) {}

func (a *lateAccountant) RequestEnd(pctx *Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.atEnd += len(pctx.ReportedOps())
}

func (a *lateAccountant) LateOps(*Context) func([]accountant.Operation) int {
	return func(ops []accountant.Operation) int {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.keepNone {
			return 0
		}
		a.late = append(a.late, ops...)
		return len(ops)
	}
}

// detachingPlugin starts a model call in OnRequest that reports its spend once release is closed,
// as summarize_async does, and sends Report's answer on done.
func detachingPlugin(release <-chan struct{}, done chan<- bool, call *accountant.ModelCall) *stubPlugin {
	return &stubPlugin{name: "engine", onReq: func(_ context.Context, pctx *Context) Action {
		reporter := pctx.OpReporter() // captured on the request goroutine, as cgModel does
		go func() {
			<-release
			done <- reporter.Report(accountant.Operation{Kind: accountant.KindSpend, Call: call})
		}()
		return Action{Type: Continue}
	}}
}

// cortex#2140: a model call that finishes after Run returned reaches the accountant's late
// handler, stamped with the plugin's name and Detached, and is not counted as dropped. The
// caller's ModelCall is not changed.
func TestRun_LateSpendGoesToTheLateHandler(t *testing.T) {
	acc := &lateAccountant{}
	release, done := make(chan struct{}), make(chan bool)
	call := &accountant.ModelCall{Model: "summarizer"}
	p, err := New([]Plugin{detachingPlugin(release, done, call)}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	before := LateOpsDropped()
	if act := p.Run(context.Background(), &Context{Body: []byte("x")}); act.Type != Continue {
		t.Fatalf("action = %v", act.Type)
	}
	close(release)
	if !<-done {
		t.Error("Report = false, want true: the late handler kept the op")
	}
	if acc.atEnd != 0 {
		t.Errorf("RequestEnd saw %d ops, want 0: the call finished after it", acc.atEnd)
	}
	if len(acc.late) != 1 {
		t.Fatalf("late handler got %d ops, want 1", len(acc.late))
	}
	op := acc.late[0]
	if op.Operator != "engine" || op.Call == nil || !op.Call.Detached || op.Call.Model != "summarizer" {
		t.Errorf("late op = %+v (call %+v), want operator engine and a Detached call", op, op.Call)
	}
	if call.Detached {
		t.Error("the caller's ModelCall was changed")
	}
	if d := LateOpsDropped() - before; d != 0 {
		t.Errorf("LateOpsDropped grew by %d, want 0", d)
	}
}

// With no LateOpsAccountant, or a handler that keeps nothing, a late op is dropped as before:
// Report says false and LateOpsDropped counts it.
func TestRun_LateSpendWithNoHandlerIsDropped(t *testing.T) {
	for name, acc := range map[string]RequestAccountant{
		"no capability": &fakeAccountant{},
		"keeps none":    &lateAccountant{keepNone: true},
	} {
		t.Run(name, func(t *testing.T) {
			release, done := make(chan struct{}), make(chan bool)
			p, err := New([]Plugin{detachingPlugin(release, done, &accountant.ModelCall{})}, WithAccountant(acc))
			if err != nil {
				t.Fatal(err)
			}
			before := LateOpsDropped()
			p.Run(context.Background(), &Context{Body: []byte("x")})
			close(release)
			if <-done {
				t.Error("Report = true, want false")
			}
			if d := LateOpsDropped() - before; d != 1 {
				t.Errorf("LateOpsDropped grew by %d, want 1", d)
			}
		})
	}
}

// Ops reported while Run seals the list each reach exactly one of RequestEnd and the late
// handler: none is lost and none is booked twice. Run with -race.
func TestRun_SealSplitsOpsExactlyOnce(t *testing.T) {
	const n = 64
	acc := &lateAccountant{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	plugin := &stubPlugin{name: "engine", onReq: func(_ context.Context, pctx *Context) Action {
		reporter := pctx.OpReporter()
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				reporter.Report(accountant.Operation{Kind: accountant.KindSpend, Call: &accountant.ModelCall{}})
			}()
		}
		close(start)
		return Action{Type: Continue}
	}}
	p, err := New([]Plugin{plugin}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &Context{Body: []byte("x")}
	p.Run(context.Background(), pctx)
	wg.Wait()
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if got := len(pctx.ReportedOps()) + len(acc.late); got != n {
		t.Errorf("ops in the list %d + late %d = %d, want %d", len(pctx.ReportedOps()), len(acc.late), got, n)
	}
	if acc.atEnd > len(pctx.ReportedOps()) {
		t.Errorf("RequestEnd saw %d ops, more than the sealed list holds (%d)", acc.atEnd, len(pctx.ReportedOps()))
	}
}
