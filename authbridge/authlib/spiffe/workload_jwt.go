package spiffe

import (
	"context"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// JWTSource is the per-fetch JWT-SVID interface satisfied by workloadJWT.
// Identical signature to authlib/plugins/tokenexchange/spiffe.JWTSource —
// the two declarations live in separate packages to avoid an import
// cycle (the framework spiffe package would otherwise have to import
// the tokenexchange-internal package), and Go's structural typing lets
// one implementation satisfy both interfaces.
type JWTSource interface {
	FetchToken(ctx context.Context) (string, error)
}

// jwtSVIDFetcher captures just the one method workloadJWT needs from
// *workloadapi.JWTSource. Defining it here lets tests substitute a hand
// rolled fake without depending on go-spiffe's internal fakeworkloadapi
// package (which is not importable from outside the SDK). Mirrors the
// x509SVIDFetcher seam in workload_x509.go.
type jwtSVIDFetcher interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// workloadJWT fetches a JWT-SVID for a fixed audience via the SPIRE
// Workload API. The SDK's *workloadapi.JWTSource caches and refreshes
// internally, so repeated FetchToken calls within a token's validity
// window avoid the agent round-trip.
type workloadJWT struct {
	sdk      jwtSVIDFetcher
	audience string
}

// Compile-time assertion that workloadJWT satisfies JWTSource. Also
// keeps the type "used" from the linter's point of view while the
// constructor is still waiting for its Provider caller (plan task T5).
var _ JWTSource = (*workloadJWT)(nil)

// newWorkloadJWT wraps a go-spiffe JWTSource so it satisfies the local
// JWTSource interface. The audience is fixed at construction — typically
// the Keycloak token endpoint URL used for client-assertion JWTs in
// RFC 8693 token exchange.
//
// Provider.JWTSource(audience) constructs a fresh adapter on each call
// while reusing the underlying SDK client.
func newWorkloadJWT(sdk *workloadapi.JWTSource, audience string) *workloadJWT {
	return &workloadJWT{sdk: sdk, audience: audience}
}

// FetchToken returns the latest JWT-SVID for the configured audience as
// a serialized JWT string. Callers invoke this on every outbound
// exchange to pick up rotation; the underlying SDK source caches and
// refreshes the SVID transparently.
func (w *workloadJWT) FetchToken(ctx context.Context) (string, error) {
	svid, err := w.sdk.FetchJWTSVID(ctx, jwtsvid.Params{Audience: w.audience})
	if err != nil {
		return "", fmt.Errorf("workloadJWT: FetchJWTSVID(audience=%q): %w", w.audience, err)
	}
	return svid.Marshal(), nil
}
