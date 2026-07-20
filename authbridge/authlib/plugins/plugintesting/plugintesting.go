// Package plugintesting exposes stub plugin adapters for listener
// tests that need to inject pre-built *auth.Auth instances (for
// example, with a mock verifier or a mock exchanger) without going
// through the real plugins' Configure path.
//
// Production code must not import this package. It lives in a
// dedicated sub-package — rather than in authbridge/authlib/plugins —
// so that it can't be pulled into the production binary, and can't
// accidentally become an alternate public constructor API for
// jwt-validation / token-exchange. Plugins build their own auth.Auth
// from their own local config in production; this package exists
// only to keep the listener-test surface small.
package plugintesting

import (
	"context"
	"net/http"

	"github.com/rossoctl/cortex/authbridge/authlib/auth"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/routing"
)

// JWTValidationStub mimics the jwt-validation plugin's OnRequest
// behavior but takes a pre-built *auth.Auth directly. Used by
// listener tests to assert listener-level behavior (reject vs.
// continue, header handling, body buffering) without standing up a
// full Configure-driven plugin.
//
// The Name() value matches the production plugin so session events
// and pipeline introspection show identical output in tests and
// production.
type JWTValidationStub struct {
	inner            *auth.Auth
	audienceFromHost bool
}

// NewJWTValidation wraps a pre-built auth handler as a pipeline
// plugin equivalent to jwt-validation. audienceFromHost=true
// mirrors the production audience_mode:per-host path, deriving the
// expected audience from pctx.Host at request time.
func NewJWTValidation(a *auth.Auth, audienceFromHost bool) *JWTValidationStub {
	return &JWTValidationStub{inner: a, audienceFromHost: audienceFromHost}
}

func (p *JWTValidationStub) Name() string { return "jwt-validation" }

func (p *JWTValidationStub) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *JWTValidationStub) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	var audience string
	if p.audienceFromHost {
		audience = routing.ServiceNameFromHost(pctx.Host)
	}
	result := p.inner.HandleInbound(ctx, authHeader, pctx.Path, audience)
	if result.Action == auth.ActionDeny {
		code := "auth.unauthorized"
		if result.DenyStatus == http.StatusServiceUnavailable {
			code = "upstream.unreachable"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	}
	if result.Claims != nil {
		pctx.Identity = testIdentity{
			subject:  result.Claims.Subject,
			clientID: result.Claims.ClientID,
			scopes:   result.Claims.Scopes,
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// testIdentity is a minimal pipeline.Identity adapter scoped to this
// test helper. Kept separate from the jwt-validation plugin's own
// adapter so plugintesting stays self-contained and doesn't reach into
// a package-internal implementation.
type testIdentity struct {
	subject, clientID string
	scopes            []string
}

func (i testIdentity) Subject() string  { return i.subject }
func (i testIdentity) ClientID() string { return i.clientID }
func (i testIdentity) Scopes() []string { return i.scopes }

func (p *JWTValidationStub) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TokenExchangeStub mimics the token-exchange plugin's OnRequest
// behavior but takes a pre-built *auth.Auth directly. See
// JWTValidationStub's docstring for the rationale.
type TokenExchangeStub struct {
	inner *auth.Auth
}

func NewTokenExchange(a *auth.Auth) *TokenExchangeStub {
	return &TokenExchangeStub{inner: a}
}

func (p *TokenExchangeStub) Name() string { return "token-exchange" }

func (p *TokenExchangeStub) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *TokenExchangeStub) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	result := p.inner.HandleOutbound(ctx, authHeader, pctx.Host)
	switch result.Action {
	case auth.ActionDeny:
		code := "upstream.token-exchange-failed"
		if result.DenyStatus == http.StatusForbidden {
			code = "policy.forbidden"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	case auth.ActionReplaceToken:
		pctx.Headers.Set("Authorization", "Bearer "+result.Token)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenExchangeStub) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// BuildPipeline wraps a slice of plugins in a pipeline.Pipeline.
// Equivalent to pipeline.New but lives here so listener tests don't
// need to import the pipeline package just for construction.
func BuildPipeline(plugins []pipeline.Plugin, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	return pipeline.New(plugins, opts...)
}
