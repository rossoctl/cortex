package plugins

import "github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"

// claimsIdentity adapts a *validation.Claims to the pipeline.Identity
// interface that Context exposes. Kept in the plugin so the pipeline
// package doesn't import any validation-specific types.
//
// A nil *validation.Claims would cause NPEs on the accessor methods,
// so jwt-validation only wraps non-nil Claims.
type claimsIdentity struct {
	c *validation.Claims
}

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
