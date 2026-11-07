package attribution

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
)

// connResolver is fakeResolver that counts its kernel questions and answers ListenerOwner
// from a table.
type connResolver struct {
	fakeResolver
	conns     int
	asked     int
	listeners map[netip.AddrPort]peerproc.Proc
}

func (r *connResolver) ConnOwner(client, server netip.AddrPort, hints ...int32) (peerproc.Proc, error) {
	r.conns++
	return r.fakeResolver.ConnOwner(client, server, hints...)
}

func (r *connResolver) ListenerOwner(dest netip.AddrPort, _ ...int32) (peerproc.Proc, error) {
	r.asked++
	p, ok := r.listeners[dest]
	if !ok {
		return peerproc.Proc{}, peerproc.ErrNotFound
	}
	return p, nil
}

var (
	curlProc = peerproc.Proc{PID: 300, Start: time.Unix(300, 0), Exe: "/usr/bin/curl"}
	connAddr = netip.MustParseAddrPort("127.0.0.1:1")
	connKey  = ConnKey{Client: connAddr, Server: connAddr}
)

func TestChain_LooksUpOncePerMemo(t *testing.T) {
	a, _ := newTest()
	r := &connResolver{fakeResolver: fakeResolver{owner: curlProc, procs: map[int32]peerproc.Proc{300: curlProc}}}
	var m Memo
	for i := range 2 {
		chain, err := a.Chain(r, connKey, &m)
		if err != nil || len(chain) != 1 || chain[0].PID != 300 {
			t.Fatalf("call %d: %+v %v, want curl and no error", i, chain, err)
		}
	}
	if r.conns != 1 {
		t.Errorf("ConnOwner calls = %d, want 1 for one memo", r.conns)
	}
	if _, _ = a.Chain(r, connKey, &Memo{}); r.conns != 2 {
		t.Errorf("ConnOwner calls = %d, want a second memo to look up again", r.conns)
	}
}

func TestChain_AFailedLookupIsKept(t *testing.T) {
	a, _ := newTest()
	r := &connResolver{fakeResolver: fakeResolver{err: peerproc.ErrNotFound}}
	var m Memo
	if chain, err := a.Chain(r, connKey, &m); chain != nil || err == nil {
		t.Fatalf("first call: %+v %v, want no chain and the lookup's error", chain, err)
	}
	if chain, err := a.Chain(r, connKey, &m); chain != nil || err != nil {
		t.Errorf("second call: %+v %v, want no chain and no error", chain, err)
	}
	if r.conns != 1 {
		t.Errorf("ConnOwner calls = %d, want 1", r.conns)
	}
}

func TestChain_AnInvalidKeyLooksNothingUp(t *testing.T) {
	a, _ := newTest()
	r := &connResolver{fakeResolver: fakeResolver{owner: curlProc}}
	for _, key := range []ConnKey{{}, {Client: connAddr}, {Server: connAddr}} {
		if chain, err := a.Chain(r, key, &Memo{}); chain != nil || err != nil {
			t.Errorf("key %+v: %+v %v, want nothing", key, chain, err)
		}
	}
	if r.conns != 0 {
		t.Errorf("ConnOwner calls = %d, want 0", r.conns)
	}
}

func TestConnKeyOf(t *testing.T) {
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 47600}
	withLocal := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
	}
	r := withLocal(httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	r.RemoteAddr = "127.0.0.1:5000"
	want := ConnKey{Client: netip.MustParseAddrPort("127.0.0.1:5000"), Server: netip.MustParseAddrPort("127.0.0.1:47600")}
	if key, ok := ConnKeyOf(r); !ok || key != want {
		t.Errorf("ConnKeyOf = %+v %v, want %+v", key, ok, want)
	}

	noLocal := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	noLocal.RemoteAddr = "127.0.0.1:5000"
	if _, ok := ConnKeyOf(noLocal); ok {
		t.Error("no LocalAddrContextKey: ok, want false")
	}

	bad := withLocal(httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	bad.RemoteAddr = "not-an-address"
	if _, ok := ConnKeyOf(bad); ok {
		t.Error("a bad RemoteAddr: ok, want false")
	}
}

func TestProgram(t *testing.T) {
	a, _ := newTest()
	if _, ok := a.Program(nil); ok {
		t.Error("an empty chain: ok, want false")
	}
	if _, ok := a.Program([]Proc{{PID: 300, Start: 1}}); ok {
		t.Error("an unknown executable: ok, want false")
	}
	a.Resolve(req(claudeUA, "s1"), pchain(100)) // 100 names a session: an agent process
	p, ok := a.Program(pchain(300, 100))
	if !ok || p.Exe != "/bin/p300" || p.PID != 300 || p.Start != 300_000 || p.Agent != "/bin/p100" {
		t.Errorf("Program = %+v %v, want p300 under agent p100", p, ok)
	}
	if p, ok := a.Program(pchain(300, 200)); !ok || p.Agent != "" {
		t.Errorf("no agent in the chain: %+v %v, want no Agent", p, ok)
	}
}

// ownService is a listener started by the client pchain(100) and running its executable:
// with an agent's User-Agent, the agent's own service.
func ownService(exe string) peerproc.Proc {
	return peerproc.Proc{PID: 101, PPID: 100, Start: time.Unix(0, 101_000), Exe: exe}
}

func TestSelfTraffic_ListenerAnswerIsCachedFor30s(t *testing.T) {
	a, clk := newTest()
	hit, miss := netip.MustParseAddrPort("127.0.0.1:4096"), netip.MustParseAddrPort("127.0.0.1:4097")
	r := &connResolver{listeners: map[netip.AddrPort]peerproc.Proc{hit: ownService("/bin/p100")}}
	var m Memo
	chain, h := pchain(100), req(claudeUA, "")

	check := func(dest netip.AddrPort, wantOK bool, wantAsked int) {
		t.Helper()
		svc, pid, ok := a.SelfTraffic(r, &m, chain, []netip.AddrPort{dest}, h)
		if ok != wantOK || (ok && (svc != dest || pid != 101)) {
			t.Errorf("%v: %v %d %v, want ok=%v", dest, svc, pid, ok, wantOK)
		}
		if r.asked != wantAsked {
			t.Errorf("%v: ListenerOwner calls = %d, want %d", dest, r.asked, wantAsked)
		}
	}
	check(hit, true, 1)
	clk.advance(29 * time.Second)
	check(hit, true, 1)
	clk.advance(2 * time.Second)
	check(hit, true, 2)

	check(miss, false, 3)
	clk.advance(29 * time.Second)
	check(miss, false, 3)
	clk.advance(2 * time.Second)
	check(miss, false, 4)
}

func TestSelfTraffic_EveryListenerMustQualify(t *testing.T) {
	a, _ := newTest()
	v4, v6 := netip.MustParseAddrPort("127.0.0.1:4096"), netip.MustParseAddrPort("[::1]:4096")
	r := &connResolver{listeners: map[netip.AddrPort]peerproc.Proc{v4: ownService("/bin/p100"), v6: ownService("/usr/bin/node")}}
	chain, h := pchain(100), req(claudeUA, "")
	if _, _, ok := a.SelfTraffic(r, &Memo{}, chain, []netip.AddrPort{v4}, h); !ok {
		t.Fatal("the agent's own listener alone: false, want true")
	}
	if _, _, ok := a.SelfTraffic(r, &Memo{}, chain, []netip.AddrPort{v4, v6}, h); ok {
		t.Error("one listener runs another executable: true, want false")
	}
	// An empty chain returns before it reads the Attributor, which a caller without a store lacks.
	if _, _, ok := (*Attributor)(nil).SelfTraffic(r, &Memo{}, nil, []netip.AddrPort{v4}, h); ok {
		t.Error("an empty chain: true, want false")
	}
}
