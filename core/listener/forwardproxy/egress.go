package forwardproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

// NewEgressHandler is the praxis-mode egress listener (spec Section 4, throwaway at phase B).
//
// cortex-gateway forwards each recorded request here with X-Cortex-Dest and
// X-Cortex-Token. The handler checks the token, dials the origin that Dest
// names through upstream, and relays the response with no buffering.
func NewEgressHandler(token string, upstream http.RoundTripper) http.Handler {
	rp := &httputil.ReverseProxy{
		Transport:     upstream,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			dest, _ := parseEgressDest(pr.In.Header.Get(headerDest)) // ServeHTTP checked it
			pr.Out.URL.Scheme = dest.Scheme
			pr.Out.URL.Host = dest.Host
			pr.Out.Host = pr.In.Host
			// Every hop and attribution header, not only Dest and Token: an origin must not
			// get the session id or process names when the gateway's strip list is wrong.
			stripHopHeaders(pr.Out.Header)
			attribution.Strip(pr.Out.Header)
			pr.Out.Header.Del(headerEgressError)
		},
		// The kind header is the egress listener's own signal. An origin must not set it.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(headerEgressError)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			dest := pipeline.SanitizeLogValue(r.Header.Get(headerDest))
			var refusal *egressguard.Refusal
			if errors.As(err, &refusal) {
				slog.Warn("egress: destination refused", "dest", dest, "error", pipeline.SanitizeLogValue(err.Error()))
				w.Header().Set(headerEgressError, "destination_private")
				// The same body as direct mode (serveOutbound), so the client sees one refusal shape.
				httpx.WriteRejection(w, pipeline.DenyStatus(http.StatusForbidden, "destination_private",
					fmt.Sprintf("refusing to reach private destination %s (%s)", refusal.Host, refusal.Addr)))
				return
			}
			if r.Context().Err() != nil {
				return // the client went away; nobody reads a status
			}
			fail := model.TransportError(err)
			slog.Warn("egress: upstream failed", "dest", dest, "kind", fail.Kind, "error", pipeline.SanitizeLogValue(fail.Message))
			w.Header().Set(headerEgressError, fail.Kind)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenMatches(r.Header.Get(headerToken), token) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		raw := r.Header.Get(headerDest)
		if raw == probeDest {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, ok := parseEgressDest(raw); !ok {
			slog.Warn("egress: bad destination", "dest", pipeline.SanitizeLogValue(raw))
			http.Error(w, "bad "+headerDest, http.StatusBadRequest)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// parseEgressDest accepts only "http://host:port" and "https://host:port" (Decision 1).
func parseEgressDest(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || port == "" {
		return nil, false
	}
	return u, true
}
