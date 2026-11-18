package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"strings"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// The gRPC receive limit THIS binary actually runs with.
//
// newExtProcServer is the only grpc.NewServer call here and is passed no
// options, so grpc-go's 4 MiB default MaxRecvMsgSize is the real ceiling on
// every ProcessingRequest Envoy sends — and in envoy-sidecar mode it is the
// third limit a large body meets, behind Envoy's
// per_connection_buffer_limit_bytes and ahead of the listener's own 1 MiB cap.
// core/listener/parity/grpclimit_test.go documents the three together and pins
// what the listener does under each.
//
// WHY THAT TEST IS NOT ENOUGH, AND WHY THIS ONE IS HERE. The parity package
// cannot import package main, so it builds its own bare grpc.NewServer() and
// reasons that production matches. It therefore pins grpc-go's DEFAULT; it
// cannot see this binary's construction. Adding one option here —
// grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20)) — moves the real ceiling and
// leaves every assertion over there green, while four comments in that package
// go on saying "a bare grpc.NewServer() and no options". This test is the half
// that fails when that happens.
//
// It asserts the transport's own refusal rather than inspecting options, for two
// reasons: grpc.Server exposes no accessor for its receive limit, and a check on
// the ServerOption slice would pass a server handed the option twice or
// overridden elsewhere. Reaching newExtProcServer rather than startGRPCExtProc
// is not a weaker target — options cannot be added to a grpc.Server after
// construction, so that function is where the limit is decided, and it avoids
// needing a listening socket at a fixed address (see bufconn below).
//
// RAISING THE LIMIT IS NOT FORBIDDEN — it is one of the three things a cap
// unification has to raise, and #864's 5 MiB Claude Code requests are the reason
// to. What is forbidden is raising it silently: this failing is the signal to
// update the cap table in core/listener/parity and this test's own figure
// together.
func TestStartGRPCExtProcKeepsDefaultRecvLimit(t *testing.T) {
	// grpc-go's default, as a number, because that is what the transport names
	// in the error it returns.
	const wantLimit = "4194304"

	empty, err := plugins.BuildWithDeps(nil, plugins.Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps(nil): %v", err)
	}

	// Empty pipelines both ways and no session store: the refusal happens in the
	// transport, before the handler reads a message, so nothing below depends on
	// what the listener would have done with the body.
	srv := newExtProcServer(pipeline.NewHolder(empty), pipeline.NewHolder(empty), nil, nil, nil)
	t.Cleanup(srv.Stop)

	// bufconn rather than startGRPCExtProc's own net.Listen. That function
	// resolves the address inside the goroutine it starts and log.Fatalf's on
	// failure, so a test driving it would have to reserve a port, release it, and
	// hand over the address — and losing that window to another process takes the
	// test BINARY down rather than failing a test. An in-memory listener has no
	// address to lose and no accept to wait for; what is under test is the server
	// the binary builds, not the socket it binds.
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := extprocv3.NewExternalProcessorClient(conn).Process(t.Context(), grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("opening the Process stream: %v", err)
	}

	// 5 MiB: over grpc-go's 4 MiB default and in the band #864 measured. The
	// Send is not asserted on — grpc-go does not police an outgoing message
	// against the peer's limit, so the refusal arrives as the stream's status on
	// Recv.
	_ = stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
		RequestBody: &extprocv3.HttpBody{Body: make([]byte, 5<<20), EndOfStream: true},
	}})

	_, err = stream.Recv()
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), wantLimit) {
		t.Fatalf("this binary's ext_proc server no longer refuses a 5 MiB message at grpc-go's %s-byte default; "+
			"if that is intentional, update the cap table in core/listener/parity and the figure here together: %v",
			wantLimit, err)
	}
}

// The test above reaches newExtProcServer, so it only describes what runs in
// production for as long as that function is where this binary's gRPC server
// comes from. A second grpc.NewServer added anywhere here — back inside
// startGRPCExtProc, or for some future second service — would be built with
// whatever options its author chose and would be invisible to every assertion
// above. This is the test that fails then.
//
// Asserted against the parsed source rather than behaviour, following
// TestCostLedgerInertClaim_NoLedgerOrAggregatorIsLinkedHere in main_test.go: the
// property IS a uniqueness claim about the source, and there is no seam to
// observe it through. A second server need not listen anywhere to break the
// claim. Parsed rather than grepped so the call in this comment, and the one in
// the limit test's own doc comment, cannot trip it.
func TestExtProcServerIsTheOnlyGRPCServerConstruction(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// FIRST, because the matcher below is the kind that can go stale silently: it
	// looks for a selector on the identifier "grpc", so renaming the import alias
	// makes it find nothing — and "found it in []" reads as though the function
	// vanished rather than as though the test went blind. Checked before the match
	// rather than after it because the match t.Fatalf's on zero hits, which would
	// abort before a check placed afterwards ever ran. (Verified by renaming the
	// alias across main.go: the clearer message only appears from here.)
	const wantAlias = "grpc"
	found := false
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "google.golang.org/grpc" {
			continue
		}
		found = true
		if imp.Name != nil && imp.Name.Name != wantAlias {
			t.Fatalf("main.go imports google.golang.org/grpc as %q, but this guard matches on %q, "+
				"so it can no longer see any construction site — update wantAlias below", imp.Name.Name, wantAlias)
		}
	}
	if !found {
		t.Fatalf("main.go no longer imports google.golang.org/grpc, so this guard matches nothing")
	}

	// Collected rather than counted, so a failure names the function that has to
	// be looked at.
	var sites []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewServer" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "grpc" {
				return true
			}
			sites = append(sites, fn.Name.Name)
			return true
		})
	}

	if len(sites) != 1 || sites[0] != "newExtProcServer" {
		t.Fatalf("grpc.NewServer must be called exactly once in main.go, in newExtProcServer; found it in %v. "+
			"TestStartGRPCExtProcKeepsDefaultRecvLimit pins the receive limit of THAT server only, so another "+
			"construction site is an unpinned ceiling: fold it into newExtProcServer, or extend the test to cover it",
			sites)
	}
}
