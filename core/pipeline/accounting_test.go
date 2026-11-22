package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/rossoctl/accountant"
)

// fakeAccountant records what the pipeline handed it, at the moment it handed it.
type fakeAccountant struct {
	calls       []string
	startClient string // pctx.ClientBody() at RequestStart
	endClient   string // pctx.ClientBody() at RequestEnd
	endBody     string // pctx.Body at RequestEnd
	pluginsRun  int    // how many plugins had run when RequestStart was called
	ran         *int
}

func (f *fakeAccountant) RequestStart(pctx *Context) {
	f.calls = append(f.calls, "start")
	f.startClient = string(pctx.ClientBody())
	if f.ran != nil {
		f.pluginsRun = *f.ran
	}
}

func (f *fakeAccountant) RequestEnd(pctx *Context) {
	f.calls = append(f.calls, "end")
	f.endClient = string(pctx.ClientBody())
	f.endBody = string(pctx.Body)
}

func TestRun_CallsStartBeforeFirstPluginAndEndAfterLast(t *testing.T) {
	ran := 0
	counting := func(name string, next *stubPlugin) *stubPlugin {
		return &stubPlugin{name: name, caps: next.caps, onReq: func(ctx context.Context, pctx *Context) Action {
			ran++
			return next.onReq(ctx, pctx)
		}}
	}
	acc := &fakeAccountant{ran: &ran}
	p, err := New([]Plugin{counting("a", appender("a", "+a")), counting("b", appender("b", "+b"))}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &Context{Body: []byte("client")}
	if act := p.Run(context.Background(), pctx); act.Type != Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if got := len(acc.calls); got != 2 || acc.calls[0] != "start" || acc.calls[1] != "end" {
		t.Fatalf("calls = %v, want [start end]", acc.calls)
	}
	if acc.pluginsRun != 0 {
		t.Errorf("RequestStart ran after %d plugins, want before the first", acc.pluginsRun)
	}
	if acc.startClient != "client" || acc.endClient != "client" {
		t.Errorf("ClientBody = %q at start, %q at end; want the body before any plugin", acc.startClient, acc.endClient)
	}
	if acc.endBody != "client+a+b" {
		t.Errorf("pctx.Body at RequestEnd = %q, want the mutated body", acc.endBody)
	}
}

// Review Focus 1: a request a plugin rejects gets RequestStart but no RequestEnd, so no
// accounting work is started for a request that never went upstream.
func TestRun_RejectSkipsEnd(t *testing.T) {
	acc := &fakeAccountant{}
	deny := &stubPlugin{name: "deny", onReq: func(context.Context, *Context) Action {
		return Deny("no", "no")
	}}
	p, err := New([]Plugin{deny}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	if act := p.Run(context.Background(), &Context{Body: []byte("x")}); act.Type != Reject {
		t.Fatalf("action = %v, want Reject", act.Type)
	}
	if len(acc.calls) != 1 || acc.calls[0] != "start" {
		t.Fatalf("calls = %v, want [start]", acc.calls)
	}
}

func TestRun_CancelSkipsEnd(t *testing.T) {
	acc := &fakeAccountant{}
	p, err := New([]Plugin{&stubPlugin{name: "a"}}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if act := p.Run(ctx, &Context{}); act.Type != Reject {
		t.Fatalf("action = %v, want Reject", act.Type)
	}
	if len(acc.calls) != 1 || acc.calls[0] != "start" {
		t.Fatalf("calls = %v, want [start]", acc.calls)
	}
}

func TestRun_NoAccountantKeepsNoClientBody(t *testing.T) {
	p, err := New([]Plugin{appender("a", "+a")})
	if err != nil {
		t.Fatal(err)
	}
	pctx := &Context{Body: []byte("client")}
	p.Run(context.Background(), pctx)
	if pctx.ClientBody() != nil {
		t.Errorf("ClientBody = %q with no accountant, want nil", pctx.ClientBody())
	}
}

func TestReportOps_StampsOperatorAndDryRun(t *testing.T) {
	report := func(name string, ops ...accountant.Operation) *stubPlugin {
		return &stubPlugin{name: name, onReq: func(_ context.Context, pctx *Context) Action {
			pctx.ReportOps(ops...)
			return Action{Type: Continue}
		}}
	}
	p, err := New([]Plugin{
		report("p1", accountant.Operation{ID: "1"}, accountant.Operation{ID: "2", Operator: "p1.sub"}),
		report("p2", accountant.Operation{ID: "3"}),
	}, WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &Context{}
	p.Run(context.Background(), pctx)
	ops := pctx.ReportedOps()
	if len(ops) != 3 {
		t.Fatalf("ops = %+v, want 3", ops)
	}
	want := []struct {
		id, operator string
		dry          bool
	}{{"1", "p1", false}, {"2", "p1.sub", false}, {"3", "p2", true}}
	for i, w := range want {
		if ops[i].ID != w.id || ops[i].Operator != w.operator || ops[i].DryRun != w.dry {
			t.Errorf("op %d = {ID:%q Operator:%q DryRun:%v}, want {%q %q %v}",
				i, ops[i].ID, ops[i].Operator, ops[i].DryRun, w.id, w.operator, w.dry)
		}
	}
	// A copy: a caller cannot change what the pipeline recorded.
	ops[0].Operator = "changed"
	if pctx.ReportedOps()[0].Operator != "p1" {
		t.Error("ReportedOps returned the context's own slice")
	}
}

func TestReportOps_KeepsAnExplicitDryRun(t *testing.T) {
	pctx := &Context{}
	pctx.setCurrent("p", InvocationPhaseRequest, ErrorPolicyEnforce)
	pctx.ReportOps(accountant.Operation{DryRun: true})
	pctx.clearCurrent()
	if !pctx.ReportedOps()[0].DryRun {
		t.Error("an op the plugin marked DryRun lost the mark")
	}
}

func TestNeedsRequestBody_TrueWithAccountant(t *testing.T) {
	plain, err := New([]Plugin{&stubPlugin{name: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if plain.NeedsRequestBody() {
		t.Fatal("a pipeline with no body reader needs no request body")
	}
	acc, err := New([]Plugin{&stubPlugin{name: "a"}}, WithAccountant(&fakeAccountant{}))
	if err != nil {
		t.Fatal(err)
	}
	if !acc.NeedsRequestBody() {
		t.Error("an accountant needs the request body buffered")
	}
}

// The client body is the slice the listener handed in, not a copy: that is safe only while no
// mutator writes into that slice, and SetBody replaces the slice rather than writing it.
func TestClientBody_NotAliasedByMutator(t *testing.T) {
	acc := &fakeAccountant{}
	p, err := New([]Plugin{appender("a", "+a")}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	// Spare capacity, so an in-place append would write into the client's backing array.
	client := make([]byte, 6, 64)
	copy(client, "client")
	pctx := &Context{Body: client}
	p.Run(context.Background(), pctx)
	if got := string(pctx.ClientBody()); got != "client" {
		t.Errorf("ClientBody = %q, want %q", got, "client")
	}
	if got := string(client[:cap(client)][:8]); got[6:] != "\x00\x00" {
		t.Errorf("the mutator wrote into the client body's backing array: %q", got)
	}
}

func TestAccountingThread(t *testing.T) {
	pctx := &Context{}
	if _, _, ok := pctx.AccountingThread(); ok {
		t.Fatal("ok before any SetAccountingThread")
	}
	pctx.SetAccountingThread("t1", "main")
	id, parent, ok := pctx.AccountingThread()
	if !ok || id != "t1" || parent != "main" {
		t.Errorf("AccountingThread = %q %q %v", id, parent, ok)
	}
	pctx.SetAccountingThread("", "x")
	if _, _, ok := pctx.AccountingThread(); !ok {
		t.Error("an empty id must not clear a thread set before")
	}
}

// An engine's model calls run on goroutines of their own while the plugin is still dispatched
// (context-guru's extract stage runs six at once). OpReporter.Report must be safe beside them,
// beside the framework's own stamping on the request goroutine, and beside ReportedOps. Run with
// -race.
func TestOpReporter_ConcurrentWithDispatch(t *testing.T) {
	const n = 16
	pctx := &Context{}
	reporter := OpReporter{c: pctx, operator: "engine", dryRun: true}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !reporter.Report(accountant.Operation{Kind: accountant.KindSpend}) {
				t.Error("OpReporter.Report dropped an op before the list was sealed")
			}
		}()
	}
	close(start)
	for i := 0; i < 50; i++ {
		pctx.setCurrent("other", InvocationPhaseRequest, ErrorPolicyEnforce)
		pctx.ReportOps(accountant.Operation{ID: "main"})
		_ = pctx.ReportedOps()
		pctx.clearCurrent()
	}
	wg.Wait()
	var spends int
	for _, op := range pctx.ReportedOps() {
		if op.Kind != accountant.KindSpend {
			continue
		}
		spends++
		if op.Operator != "engine" || !op.DryRun {
			t.Errorf("op = {Operator:%q DryRun:%v}, want the operator and dry run it was reported with", op.Operator, op.DryRun)
		}
	}
	if spends != n {
		t.Errorf("spend ops = %d, want %d", spends, n)
	}
}

// OpReporter.Report takes the operator and dry-run state the reporter was built with and reads
// nothing of the plugin being dispatched, so an op reported after the plugin returned is not
// given another's name.
func TestOpReporter_ReadsNoDispatchState(t *testing.T) {
	pctx := &Context{}
	pctx.setCurrent("someone-else", InvocationPhaseRequest, ErrorPolicyObserve)
	reporter := OpReporter{c: pctx, operator: "engine", dryRun: false}
	reporter.Report(accountant.Operation{ID: "1"}, accountant.Operation{ID: "2", Operator: "engine.sub", DryRun: true})
	pctx.clearCurrent()
	ops := pctx.ReportedOps()
	if len(ops) != 2 {
		t.Fatalf("ops = %+v, want 2", ops)
	}
	if ops[0].Operator != "engine" || ops[0].DryRun {
		t.Errorf("op 0 = {Operator:%q DryRun:%v}, want {engine false}", ops[0].Operator, ops[0].DryRun)
	}
	if ops[1].Operator != "engine.sub" || !ops[1].DryRun {
		t.Errorf("op 1 = {Operator:%q DryRun:%v}, want its own operator and dry run kept", ops[1].Operator, ops[1].DryRun)
	}
}

// Run seals the op list right before RequestEnd hands it to the accountant. An op reported after
// that (a detached model call, a spec non-goal) is dropped and counted, not appended to a list
// nobody reads again.
func TestReportOps_AfterRunIsDroppedAndCounted(t *testing.T) {
	var held *Context
	keep := &stubPlugin{name: "keep", onReq: func(_ context.Context, pctx *Context) Action {
		held = pctx
		pctx.ReportOps(accountant.Operation{ID: "in-request"})
		return Action{Type: Continue}
	}}
	acc := &fakeAccountant{}
	p, err := New([]Plugin{keep}, WithAccountant(acc))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &Context{Body: []byte("x")}
	p.Run(context.Background(), pctx)

	before := LateOpsDropped()
	reporter := OpReporter{c: held, operator: "keep", dryRun: false}
	done := make(chan bool)
	go func() { done <- reporter.Report(accountant.Operation{ID: "late"}) }()
	if <-done {
		t.Error("OpReporter.Report after Run returned true, want the op dropped")
	}
	held.ReportOps(accountant.Operation{ID: "late-2"})
	if got := LateOpsDropped() - before; got != 2 {
		t.Errorf("LateOpsDropped grew by %d, want 2", got)
	}
	ops := pctx.ReportedOps()
	if len(ops) != 1 || ops[0].ID != "in-request" {
		t.Errorf("ops = %+v, want only the in-request op", ops)
	}
}
