package forwardproxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Hop sends each recorded praxis-mode request to cortex-gateway at the dial (#1721).
// Only the dial target moves: r.Host and the path stay the client's, and the real
// destination goes in X-Cortex-Dest. The gateway records the rows of a request the
// hop owns, so the forward proxy records none for it.
type Hop struct {
	gateway string
	token   string
	client  *http.Client
	ready   atomic.Bool
	owned   sync.Map // *pipeline.Context -> attribution.Wire

	// ttl is how long a join or a cut is kept; hopJoinTTL except in tests.
	ttl    time.Duration
	joinMu sync.Mutex
	joins  map[string]*hopJoin // request id -> bridged tunnel that waits for its row
	cuts   map[string]hopCut   // request id -> Cortex's own cut
}

// NewHop returns a Hop to the gateway at gatewayAddr (host:port). It is not ready
// until Probe gets a 204.
func NewHop(gatewayAddr, token string) *Hop {
	return &Hop{
		gateway: gatewayAddr,
		token:   token,
		ttl:     hopJoinTTL,
		joins:   map[string]*hopJoin{},
		cuts:    map[string]hopCut{},
		client: &http.Client{
			// HTTP/1, no proxy env, and Go's default compression.
			Transport: &http.Transport{
				Proxy:               nil,
				ForceAttemptHTTP2:   false,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Ready reports whether a probe has passed.
func (h *Hop) Ready() bool { return h.ready.Load() }

// Probe sends a probe every `every` until one gets 204 or ctx ends. Logs once when ready.
func (h *Hop) Probe(ctx context.Context, every time.Duration) {
	var lastWarn time.Time
	for {
		status, err := h.probeOnce(ctx)
		if status == http.StatusNoContent {
			h.ready.Store(true)
			slog.Info("praxis hop: cortex-gateway is ready", "gateway", h.gateway)
			return
		}
		if time.Since(lastWarn) >= 30*time.Second {
			lastWarn = time.Now()
			slog.Warn("praxis hop: cortex-gateway is not ready", "gateway", h.gateway, "status", status, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (h *Hop) probeOnce(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.gateway+"/", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(headerToken, h.token)
	req.Header.Set(headerProbe, "1")
	req.Header.Set(headerDest, probeDest)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// own marks pctx as a request the hop sends, with its attribution w (branch point 2).
func (h *Hop) own(pctx *pipeline.Context, w attribution.Wire) { h.owned.Store(pctx, w) }

// release removes the mark own set.
func (h *Hop) release(pctx *pipeline.Context) { h.owned.Delete(pctx) }

// owns gives the attribution of a request the hop sends.
func (h *Hop) owns(pctx *pipeline.Context) (attribution.Wire, bool) {
	v, ok := h.owned.Load(pctx)
	if !ok {
		return attribution.Wire{}, false
	}
	return v.(attribution.Wire), true
}

// hopWire is the attribution the hop sends for pctx, read after hydration (Decision 8).
func hopWire(pctx *pipeline.Context, clientHeaders http.Header, recordSession string) attribution.Wire {
	return attribution.Wire{
		Agent:         attribution.AgentOf(clientHeaders),
		Proc:          pctx.AttributedProc(),
		ClientProc:    pctx.AttributedClientProc(),
		Session:       pctx.SessionID,
		Source:        string(pctx.SessionSource),
		SessionHeader: pctx.NamingHeader(), // the pinned value direct mode records; a plugin can write pctx.SessionHeader
		ChainSession:  pctx.ChainSession,
		RecordSession: recordSession,
	}
}

// hopDest is X-Cortex-Dest for u: <scheme>://<host>:<port>, with the port always present.
func hopDest(u *http.Request) string {
	port := u.URL.Port()
	if port == "" {
		port = "80"
		if u.URL.Scheme == "https" {
			port = "443"
		}
	}
	return u.URL.Scheme + "://" + net.JoinHostPort(u.URL.Hostname(), port)
}

// hopDo sends r to the gateway in place of client.Do (branch point 4). When the
// gateway is not ready or refuses the dial, it answers 503 gateway_unavailable and
// records the request row and the 503 row itself, with answered true.
func (s *Server) hopDo(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid string) (resp *http.Response, err error, answered bool) {
	h := s.Hop
	if !h.Ready() {
		s.gatewayUnavailable(w, r, pctx, tl, sid, "not ready")
		return nil, nil, true
	}
	wire, _ := h.owns(pctx) // zero when there is no store: nothing records then
	r.Header.Set(headerDest, hopDest(r))
	r.Header.Set(headerToken, h.token)
	r.Header.Set(headerRequestID, pctx.RequestID())
	r.Header.Set(attribution.HeaderAttribution, wire.Encode())
	r.URL.Scheme, r.URL.Host = "http", h.gateway
	// Hold a bridged tunnel until ingestion gives this request's row (Task 6).
	h.join(pctx.RequestID(), tl)
	resp, err = h.client.Do(r)
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		s.gatewayUnavailable(w, r, pctx, tl, sid, model.TransportError(err).Message)
		h.dropJoin(pctx.RequestID())
		return nil, nil, true
	}
	return resp, err, false
}

// gatewayUnavailable answers 503 and records both rows, because the gateway never saw
// the request (Decision 4).
func (s *Server) gatewayUnavailable(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid, cause string) {
	// Release first, so branch point 5 records the 503 row.
	s.Hop.release(pctx)
	slog.Debug("praxis hop: cortex-gateway is unavailable", "host", pipeline.SanitizeLogValue(r.Host), "cause", cause)
	if s.Sessions != nil {
		s.recordCapturedRequest(tl, pctx, sid, nil)
	}
	http.Error(w, `{"error":"gateway unavailable"}`, http.StatusServiceUnavailable)
	s.recordOutboundResponseEvent(pctx, http.StatusServiceUnavailable, &pipeline.EventError{
		Kind:    pipeline.ErrorKindGatewayUnavailable,
		Message: "cortex-gateway is unavailable: " + cause,
	})
}
