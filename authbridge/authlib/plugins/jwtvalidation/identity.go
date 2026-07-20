package jwtvalidation

import (
	"strconv"
	"strings"

	"github.com/rossoctl/rossocortex/authbridge/authlib/contracts"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation/validation"
)

// claimsIdentity adapts a *validation.Claims to the pipeline.Identity
// interface that Context exposes. Kept in the plugin so the pipeline
// package doesn't import any validation-specific types.
//
// A nil *validation.Claims would cause NPEs on the accessor methods,
// so jwt-validation only wraps non-nil Claims.
//
// Beyond the minimal pipeline.Identity surface it also implements
// contracts.ClaimsCarrier so richer consumers (the cpex plugin) can read
// issuer / audience / auth-method / curated claims without
// pipeline.Identity growing.
type claimsIdentity struct {
	c *validation.Claims
}

// Compile-time assertion: claimsIdentity exposes the richer claim
// surface. If contracts.ClaimsCarrier gains a method, this line forces
// the adapter to keep up at `go build`.
var _ contracts.ClaimsCarrier = claimsIdentity{}

func (i claimsIdentity) Subject() string {
	if i.c == nil {
		return ""
	}
	return i.c.Subject
}

func (i claimsIdentity) ClientID() string {
	if i.c == nil {
		return ""
	}
	return i.c.ClientID
}

func (i claimsIdentity) Scopes() []string {
	if i.c == nil {
		return nil
	}
	return i.c.Scopes
}

// Issuer implements contracts.ClaimsCarrier.
func (i claimsIdentity) Issuer() string {
	if i.c == nil {
		return ""
	}
	return i.c.Issuer
}

// Audience implements contracts.ClaimsCarrier.
func (i claimsIdentity) Audience() []string {
	if i.c == nil {
		return nil
	}
	return i.c.Audience
}

// AuthMethod implements contracts.ClaimsCarrier. A claimsIdentity is only
// ever constructed from a verified JWT, so the method is always "jwt".
func (i claimsIdentity) AuthMethod() string {
	return "jwt"
}

// Claims implements contracts.ClaimsCarrier. It returns a deliberately
// SMALL, string-valued subset — issuer, audience (comma-joined), and
// expiry (Unix seconds) — never the full raw `Extra` claim map. Those
// three are the keys policy commonly branches on; dumping the entire
// claim set would risk forwarding secrets/PII across the FFI and into
// CPEX traces / the session API.
func (i claimsIdentity) Claims() map[string]string {
	if i.c == nil {
		return nil
	}
	out := make(map[string]string, 3)
	if i.c.Issuer != "" {
		out["issuer"] = i.c.Issuer
	}
	if len(i.c.Audience) > 0 {
		out["audience"] = strings.Join(i.c.Audience, ",")
	}
	if !i.c.ExpiresAt.IsZero() {
		out["exp"] = strconv.FormatInt(i.c.ExpiresAt.Unix(), 10)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
