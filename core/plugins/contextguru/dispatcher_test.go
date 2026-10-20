package contextguru

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

func TestCortexDispatcherMeetsTheContract(t *testing.T) {
	dispatchtest.Contract(t, func(t *testing.T, upstreamURL string) dispatchtest.Harness {
		d := newCortexDispatcher(newPingClient())
		return dispatchtest.Harness{D: d, Hold: func(h http.Header) dispatch.Ref {
			return d.hold("http", stripScheme(upstreamURL), h)
		}}
	})
}

// stripScheme is url.Parse's .Host, inlined: the contract hands us a full "http://host:port"
// URL and hold wants scheme and host apart.
func stripScheme(u string) string {
	const p = "http://"
	if len(u) > len(p) && u[:len(p)] == p {
		return u[len(p):]
	}
	return u
}

func TestDispatchSetsTheSentinel(t *testing.T) {
	// A ping that transits this proxy must short-circuit OnRequest, not be recorded again.
	var got http.Header
	srv := newHeaderSink(t, &got)
	d := newCortexDispatcher(newPingClient())
	ref := d.hold("http", stripScheme(srv.URL), http.Header{"Authorization": {"Bearer k"}})
	if _, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/v1/messages",
		Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got.Get(sentinelHeader) == "" {
		t.Fatalf("ping carried no %s header", sentinelHeader)
	}
}

func newHeaderSink(t *testing.T, got *http.Header) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPingClientBypassesTheProxy(t *testing.T) {
	c := newPingClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("ping client transport is %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("ping client routes through a proxy; a ping sent back into Cortex would be recorded as agent traffic")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Fatal("ping client has no IdleConnTimeout; idle connections are never reclaimed")
	}
	if c.CheckRedirect == nil {
		t.Fatal("ping client has no CheckRedirect; it will follow a redirect to another host")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// TestPingClientDoesNotFollowRedirects guards against the ping client sending a credential header
// (x-api-key / x-goog-api-key) to a second host: Go strips only Authorization/Cookie on a
// cross-host redirect, so following one would leak those headers to srv2.
func TestPingClientDoesNotFollowRedirects(t *testing.T) {
	var srv2Hit bool
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv2Hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv2.Close)

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv2.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv1.Close)

	d := newCortexDispatcher(newPingClient())
	ref := d.hold("http", stripScheme(srv1.URL), http.Header{"x-api-key": {"secret"}})
	resp, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/v1/messages", Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Do returned an error for a 307 response: %v", err)
	}
	if resp.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (the ping client must not follow the redirect)", resp.Status, http.StatusTemporaryRedirect)
	}
	if srv2Hit {
		t.Fatal("the ping client followed the redirect to the second host; a credential header would leak there")
	}
}

// Every path that drops a keep-alive entry releases its credential; Shutdown drops them all. After
// it, the plugin must hold no caller credential at all.
func TestShutdownReleasesEveryHeldRef(t *testing.T) {
	d := newCortexDispatcher(newPingClient())
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	hdr := http.Header{"Authorization": {"Bearer sk-caller"}}
	for i, s := range []string{"s1", "s1", "s2", "s2"} { // a session's FIRST request is not tracked
		key := keepalive.Key{Session: s}
		k.Record(keepalive.Record{Key: key, StartedAt: now.Add(time.Duration(i) * time.Second),
			Body:     []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
			Provider: bschemas.Anthropic,
			Endpoint: "api.anthropic.com", Route: "/v1/messages", Status: http.StatusOK,
			Usage: keepalive.Usage{CacheRead: 48576}, UsageOK: true,
			Policy: keepalive.Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
				MaxUSDPerPing: 0.25, MinPrefixTokens: 20000},
			Header: hdr, Hold: func() dispatch.Ref { return d.hold("https", "api.anthropic.com", hdr) }})
	}
	d.mu.Lock()
	held := len(d.held)
	d.mu.Unlock()
	if held != 2 {
		t.Fatalf("held %d credentials for two tracked sessions, want 2", held)
	}
	k.Shutdown()
	d.mu.Lock()
	held = len(d.held)
	d.mu.Unlock()
	if held != 0 {
		t.Fatalf("after Shutdown the plugin still holds %d credentials", held)
	}
}

// --- pricer tests (resolverPricer folded into cortexDispatcher.Price) -------------------------

type fixedResolver struct {
	rates  pricing.Rates
	gotEP  string
	gotTok int
}

func (f *fixedResolver) Resolve(ep, _ string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	f.gotEP, f.gotTok = ep, promptTotal
	return f.rates, pricing.ProvNone
}
func (f *fixedResolver) CurrencyFor(string, string) string { return "USD" }

func ratesOf(in, cw, cr, out float64) pricing.Rates {
	var r pricing.Rates
	for t, v := range map[pricing.Tier]float64{pricing.TierInput: in, pricing.TierCacheWrite: cw,
		pricing.TierCacheRead: cr, pricing.TierOutput: out} {
		r.Base[t], r.Set[t] = v, true
	}
	return r
}

func TestDispatcherPriceMapsTiersAndPassesEndpointAndPrefix(t *testing.T) {
	f := &fixedResolver{rates: ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)}
	d := &cortexDispatcher{rates: func() pricing.Resolver { return f }}
	price, ok := d.Price(context.Background(), "api.anthropic.com", "claude-sonnet-5", 48_000)
	if !ok {
		t.Fatal("priced model reported unpriced")
	}
	if price.Input != 3e-6 || price.CacheWrite != 3.75e-6 || price.CacheRead != 0.3e-6 || price.Output != 15e-6 {
		t.Fatalf("price = %+v", price)
	}
	if f.gotEP != "api.anthropic.com" || f.gotTok != 48_000 {
		t.Fatalf("Resolve got (%q, %d)", f.gotEP, f.gotTok)
	}
}

func TestDispatcherPriceUnsetTierIsUnpriced(t *testing.T) {
	r := ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)
	r.Set[pricing.TierCacheRead] = false
	d := &cortexDispatcher{rates: func() pricing.Resolver { return &fixedResolver{rates: r} }}
	if _, ok := d.Price(context.Background(), "h", "m", 1); ok {
		t.Fatal("a rate table without cache-read priced a ping")
	}
}

func TestDispatcherPriceUnsetCacheWriteFallsBackToInput(t *testing.T) {
	r := ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)
	r.Set[pricing.TierCacheWrite] = false
	d := &cortexDispatcher{rates: func() pricing.Resolver { return &fixedResolver{rates: r} }}
	price, ok := d.Price(context.Background(), "h", "m", 1)
	if !ok {
		t.Fatal("a rate table with only cache-write unset left the ping unpriced")
	}
	if price.CacheWrite != price.Input {
		t.Fatalf("CacheWrite = %v, want it to fall back to Input = %v", price.CacheWrite, price.Input)
	}
}

func TestDispatcherPriceUnsetOutputIsUnpriced(t *testing.T) {
	r := ratesOf(3e-6, 3.75e-6, 0.3e-6, 15e-6)
	r.Set[pricing.TierOutput] = false
	d := &cortexDispatcher{rates: func() pricing.Resolver { return &fixedResolver{rates: r} }}
	if _, ok := d.Price(context.Background(), "h", "m", 1); ok {
		t.Fatal("a rate table without output priced a ping")
	}
}

func TestDispatcherPriceWithNoResolver(t *testing.T) {
	d := &cortexDispatcher{rates: func() pricing.Resolver { return nil }}
	if _, ok := d.Price(context.Background(), "h", "m", 1); ok {
		t.Fatal("nil resolver priced a ping")
	}
}

// A wipeReader is not *bytes.Reader, so http.NewRequest cannot auto-detect its length — Do must
// set ContentLength itself, or the ping goes out chunked. That matters beyond style: a minimal
// HTTP server (this project's own live-verification fake upstream included) reads a request body
// by Content-Length and never dechunks, so an unset length delivers it an empty body.
func TestDoSetsContentLengthRatherThanChunking(t *testing.T) {
	var gotCL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCL = r.Header.Get("Content-Length")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	d := newCortexDispatcher(newPingClient())
	ref := d.hold("http", stripScheme(srv.URL), http.Header{})
	body := []byte(`{"model":"m","max_tokens":1,"stream":false}`)
	if _, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/v1/messages", Body: body}); err != nil {
		t.Fatal(err)
	}
	if gotCL == "" {
		t.Fatal("ping went out with no Content-Length header (chunked instead)")
	}
}

// Do's body is a wipeReader, not *bytes.Reader, so http.NewRequest cannot infer GetBody from its
// type switch either — and without GetBody, net/http never retries a write that failed on a
// reused idle connection. A ping fires every ~270s+, long enough that the server closing a pooled
// connection between pings is a live concern; losing the free retry turns that into a visible
// ping failure instead of a transparent one.
func TestDoSetsGetBodyForTransportRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	d := newCortexDispatcher(&http.Client{})
	ref := d.hold("http", stripScheme(srv.URL), http.Header{})
	body := []byte(`{"model":"m","max_tokens":1,"stream":false}`)
	captured := make(chan *http.Request, 1)
	d.client.Transport = captureTransport{inner: http.DefaultTransport, captured: captured}
	if _, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/v1/messages", Body: body}); err != nil {
		t.Fatal(err)
	}
	hreq := <-captured
	if hreq.GetBody == nil {
		t.Fatal("Do built a request with no GetBody; net/http cannot retry a failed write on a reused connection")
	}
	rc, err := hreq.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("GetBody returned %q, want %q", got, body)
	}
}

// captureTransport records the *http.Request Do actually sent, then forwards to inner.
type captureTransport struct {
	inner    http.RoundTripper
	captured chan *http.Request
}

func (c captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.captured <- r
	return c.inner.RoundTrip(r)
}

// newCortexDispatcher must refuse a redirect regardless of what client it is given — the no-
// redirect rule is a property of this dispatcher, not of remembering to pass newPingClient().
// production always does (plugin.go), but nothing in the constructor enforced it, and a GetBody
// (added above) is exactly what makes a bare *http.Client start following a POST redirect: Go's
// client cannot resend the body without GetBody and silently stops at the redirect instead, which
// is how an unenforced caller could pass this dispatcher's own contract test by accident.
func TestNewCortexDispatcherRefusesRedirectsEvenWithABareClient(t *testing.T) {
	var srv2Hit bool
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv2Hit = true
	}))
	defer srv2.Close()
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv2.URL, http.StatusTemporaryRedirect)
	}))
	defer srv1.Close()

	d := newCortexDispatcher(&http.Client{}) // deliberately bare: no CheckRedirect set by the caller
	ref := d.hold("http", stripScheme(srv1.URL), http.Header{"x-api-key": {"secret"}})
	resp, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/v1/messages", Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Do returned an error for a 307 response: %v", err)
	}
	if resp.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (the dispatcher must not follow the redirect)", resp.Status, http.StatusTemporaryRedirect)
	}
	if srv2Hit {
		t.Fatal("the dispatcher followed the redirect to the second host with a bare client; the credential header would leak there")
	}
}
