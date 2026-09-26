package contracts

// ClaimsCarrier is an optional capability interface a pipeline.Identity
// may implement to expose richer claim data than the minimal
// pipeline.Identity surface (Subject / ClientID / Scopes).
//
// It exists so consumers that want issuer / audience / auth-method /
// curated claims — chiefly the cpex plugin building a policy-input
// document — can read them without growing pipeline.Identity (which
// stays deliberately minimal so any auth shape can satisfy it). Mirrors
// the ContentSource pattern: consumers type-assert to this interface and
// simply skip the enrichment when the concrete identity doesn't
// implement it.
//
//	if cc, ok := pctx.Identity.(contracts.ClaimsCarrier); ok {
//	    authMethod = cc.AuthMethod()
//	    claims     = cc.Claims()
//	}
//
// Producers (e.g. jwt-validation's claims adapter) curate what Claims
// returns — a small, safe-to-forward set (issuer, audience, expiry), NOT
// the full raw claim map. The session API and CPEX traces both surface
// this data, so producers must keep it free of secrets.
type ClaimsCarrier interface {
	// Issuer is the token issuer (`iss`), or "" when not applicable.
	Issuer() string

	// Audience is the token audience list (`aud`), or nil.
	Audience() []string

	// AuthMethod names how the caller authenticated — "jwt", "mtls",
	// "spiffe", etc. Drives policies that branch on authentication
	// strength. "" when the producer can't classify it.
	AuthMethod() string

	// Claims returns a curated, string-valued claim set safe to forward
	// into policy context and observability surfaces. Producers pick the
	// keys (conventionally "issuer", "audience", "exp"); they MUST NOT
	// dump the full raw claim map, which may contain secrets or PII.
	// Values are strings so the set maps cleanly onto CPEX's
	// SubjectExtension.Claims (map[string]string) without lossy coercion.
	Claims() map[string]string
}
