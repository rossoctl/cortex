package contextguru

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// authHeaderNames are the slots a provider credential arrives in. Held masked, per hold, only
// while the keeper tracks that entry (keepalive.Record.Hold), and zeroized on Release.
var authHeaderNames = []string{"Authorization", "x-api-key", "x-goog-api-key"}

// cortexDispatcher is the plugin's dispatch.Dispatcher: transport and credential custody only,
// for every call the engine's core makes (keep-alive pings today). The credential is the one the
// request carried when it left Cortex's pipeline — after any token-exchange or
// credential-injection plugin ran — because OnFinish reads pctx.Headers then.
//
// Lock order: hold and Release run under the keeper's lock and take d.mu; Do runs outside the
// keeper's lock and takes only d.mu. Nothing here calls back into the keeper.
type cortexDispatcher struct {
	client *http.Client
	// rates resolves pricing.Resolver on every call: SetPricingResolver may run after Configure,
	// and a reload hands a new instance a new resolver (see Price).
	rates func() pricing.Resolver

	mu   sync.Mutex
	next uint64
	held map[dispatch.Ref]heldTarget
}

type heldTarget struct {
	scheme, host string
	auth         []heldHeader
}

// newCortexDispatcher copies c and overrides CheckRedirect unconditionally, so refusing a
// redirect is a property of this dispatcher rather than something every caller must remember by
// passing newPingClient() — nothing else about c changes.
func newCortexDispatcher(c *http.Client) *cortexDispatcher {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cortexDispatcher{client: &cc, held: map[dispatch.Ref]heldTarget{}}
}

// newPingClient is a standalone client that never routes through HTTPS_PROXY — a ping sent back
// into this proxy would be recorded as agent traffic. The sentinel header is the second guard.
//
// It never follows a redirect: Go's default client strips Authorization/Cookie on a cross-host
// redirect, but not x-api-key / x-goog-api-key, so a 307/308 could carry a credential header to a
// host the operator never intended it for. Matches core/tlsbridge/upstream.go's choice.
// IdleConnTimeout bounds how long an idle connection is kept around between pings.
func newPingClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// hold captures the upstream and the credential a finished request carried, masked, and returns
// the Ref the keeper gives back to Do and Release.
func (d *cortexDispatcher) hold(scheme, host string, reqHeaders http.Header) dispatch.Ref {
	var auth []heldHeader
	for _, name := range authHeaderNames {
		for _, v := range reqHeaders.Values(name) {
			b := []byte(v)
			credMask(b)
			auth = append(auth, heldHeader{name: name, val: b})
		}
	}
	if scheme == "" {
		scheme = "https"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	ref := dispatch.Ref("cortex:" + strconv.FormatUint(d.next, 10))
	d.held[ref] = heldTarget{scheme: scheme, host: host, auth: auth}
	return ref
}

// Close releases the ping client's idle connections. Called from Shutdown so a hot-reloaded
// instance's connections do not linger past it.
func (d *cortexDispatcher) Close() {
	d.client.CloseIdleConnections()
}

// Release runs under the keeper's lock: zeroize and forget, nothing else.
func (d *cortexDispatcher) Release(ref dispatch.Ref) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.held[ref]; ok {
		zeroHeaders(h.auth)
		delete(d.held, ref)
	}
}

// Do sends one request on a private copy of the held credential, so a Release while it is on the
// wire zeroizes the held bytes without touching the ones being sent.
func (d *cortexDispatcher) Do(ctx context.Context, req dispatch.Request) (dispatch.Response, error) {
	d.mu.Lock()
	h, ok := d.held[req.Ref]
	var auth []heldHeader
	if ok {
		auth = make([]heldHeader, len(h.auth))
		for i, a := range h.auth {
			auth[i] = a.clone()
		}
	}
	d.mu.Unlock()
	defer zeroHeaders(auth)
	if !ok || h.host == "" {
		return dispatch.Response{}, dispatch.ErrNotHeld
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	// The body is a private copy that we wipe when the transport closes it: the core zeroizes
	// req.Body once Do returns, and the transport may still be reading it then.
	body := newWipeReader(req.Body)
	hreq, err := http.NewRequestWithContext(ctx, method, h.scheme+"://"+h.host+req.Path, body)
	if err != nil {
		body.Close()
		return dispatch.Response{}, err
	}
	// wipeReader is not *bytes.Reader, so http.NewRequest cannot auto-detect its length from the
	// type switch it uses for that — left unset, the request goes out chunked, which some minimal
	// HTTP servers (reading by Content-Length only, never dechunking) see as an empty body.
	hreq.ContentLength = int64(len(req.Body))
	// wipeReader is not *bytes.Reader either, so http.NewRequest cannot infer GetBody from it —
	// and without GetBody, net/http never retries a write that failed on a reused idle
	// connection. req.Body is still live here (the core zeroes it only after Do returns), so a
	// fresh copy for a retry is safe to make on demand.
	hreq.GetBody = func() (io.ReadCloser, error) { return newWipeReader(req.Body), nil }
	for name, vs := range req.Header {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	hreq.Header.Set(sentinelHeader, "1")
	// Unmask as late as possible and wipe the buffer at once. The value becomes a Go string in
	// the header map, which cannot be overwritten — so this bounds the plaintext to this request
	// rather than eliminating it.
	for _, a := range auth {
		credMask(a.val)
		hreq.Header.Add(a.name, string(a.val))
		credZero(a.val)
	}
	resp, err := d.client.Do(hreq)
	if err != nil {
		return dispatch.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, dispatch.MaxResponseBytes))
	if err != nil {
		return dispatch.Response{Status: resp.StatusCode}, err
	}
	return dispatch.Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

// Price prices from Cortex's own rate table, so the keeper's pre-flight ceiling and the cost the
// plugin logs are the same number (spec §9.1).
//
// Input, CacheRead and Output must be set: an unset rate on any of them is unknown, not free, and
// pricing it as zero would leave the ceiling unenforced for that model. CacheWrite is the
// exception — many tables (OpenAI's included) carry no separate cache-write rate, so an unset
// CacheWrite falls back to the Input rate rather than making the whole ping unpriced.
func (d *cortexDispatcher) Price(_ context.Context, endpoint, model string, promptTokens int64) (dispatch.Price, bool) {
	if d.rates == nil || model == "" {
		return dispatch.Price{}, false
	}
	res := d.rates()
	if res == nil {
		return dispatch.Price{}, false
	}
	rates, _ := res.Resolve(endpoint, model, int(promptTokens))
	var out dispatch.Price
	for _, t := range []struct {
		tier pricing.Tier
		dst  *float64
	}{
		{pricing.TierInput, &out.Input},
		{pricing.TierCacheRead, &out.CacheRead}, {pricing.TierOutput, &out.Output},
	} {
		v, ok := rates.For(t.tier)
		if !ok {
			return dispatch.Price{}, false
		}
		*t.dst = v
	}
	if v, ok := rates.For(pricing.TierCacheWrite); ok {
		out.CacheWrite = v
	} else {
		out.CacheWrite = out.Input
	}
	return out, true
}

// wipeReader is a request body over a private copy of the caller's bytes, zeroed on Close. The
// transport always closes a request body, and only once it has finished reading it.
type wipeReader struct {
	*bytes.Reader
	buf  []byte
	once sync.Once
}

func newWipeReader(b []byte) *wipeReader {
	buf := append([]byte(nil), b...)
	return &wipeReader{Reader: bytes.NewReader(buf), buf: buf}
}

func (w *wipeReader) Close() error {
	w.once.Do(func() { credZero(w.buf) })
	return nil
}
