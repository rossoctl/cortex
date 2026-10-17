package cpex

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/core/spiffe"
)

// defaultWorkloadHeader is the identity header the workload token is
// presented under when none is configured. It is the header CPEX's
// `role: caller_workload` resolvers read by convention.
const defaultWorkloadHeader = "x-workload-token"

// workloadConfig configures workload token injection. An empty Audience
// turns it off.
type workloadConfig struct {
	Audience string `json:"audience,omitempty" description:"Audience of the JWT-SVID Cortex fetches from SPIRE for the agent. Empty turns workload token injection off."`
	Header   string `json:"header,omitempty" default:"x-workload-token" description:"Identity header the JWT-SVID is presented under; a role: caller_workload resolver reads it."`
}

// workloadSource presents the pod's own JWT-SVID, fetched from the
// SPIFFE provider, to CPEX identity resolution as the calling workload.
// The sidecar and the agent share the pod, so the pod's SVID is the
// agent's identity. Like the principal, the token is handed only to
// identity resolution and never written to the forwarded request.
type workloadSource struct {
	header string // lowercase, as the identity payload keys headers
	src    spiffe.JWTSource
}

// newWorkloadSource returns nil when injection is off. With no SPIFFE
// provider the source still drops a caller's own copy of the header,
// but injects nothing, so a route that authenticates by it denies.
func newWorkloadSource(cfg workloadConfig, src spiffe.JWTSource) *workloadSource {
	if cfg.Audience == "" {
		return nil
	}
	if src == nil {
		slog.Warn("cpex: workload.audience is set but no SPIFFE provider is configured (no spiffe: block); routes that authenticate by the workload token will deny",
			"audience", cfg.Audience)
	}
	return &workloadSource{header: strings.ToLower(strings.TrimSpace(cfg.Header)), src: src}
}

// headerName is the workload header, or "" when injection is off.
func (w *workloadSource) headerName() string {
	if w == nil {
		return ""
	}
	return w.header
}

// apply drops the caller's copy of the workload header from the identity
// headers and, with inject set, adds the pod's JWT-SVID in its place. A
// failed fetch adds nothing: the request then carries no workload
// credential, and routes that need one deny.
func (w *workloadSource) apply(ctx context.Context, headers map[string]string, inject bool) map[string]string {
	if w == nil {
		return headers
	}
	delete(headers, w.header)
	if !inject || w.src == nil {
		return headers
	}
	tok, err := w.src.FetchToken(ctx)
	if err != nil {
		slog.Warn("cpex: workload JWT-SVID unavailable", "error", err)
		return headers
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers[w.header] = tok
	return headers
}
