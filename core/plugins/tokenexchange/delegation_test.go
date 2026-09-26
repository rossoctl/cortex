package tokenexchange

import (
	"crypto/rand"
	"crypto/rsa"
	"reflect"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestSplitScopes(t *testing.T) {
	if got := splitScopes("openid github-tool-aud github-full-access"); !reflect.DeepEqual(
		got, []string{"openid", "github-tool-aud", "github-full-access"}) {
		t.Fatalf("got %v", got)
	}
	if got := splitScopes(""); got != nil {
		t.Fatalf("empty scopes should be nil, got %v", got)
	}
	if got := splitScopes("  spaced   out  "); !reflect.DeepEqual(got, []string{"spaced", "out"}) {
		t.Fatalf("extra whitespace not collapsed: %v", got)
	}
}

func TestRecordDelegationHop_AppendsExchangeHop(t *testing.T) {
	pctx := &pipeline.Context{}
	result := &auth.OutboundResult{
		Action:          auth.ActionReplaceToken,
		CacheHit:        true,
		TargetAudience:  "github-tool",
		RequestedScopes: "openid github-tool-aud",
	}
	recordDelegationHop(pctx, "", result)

	d := pctx.Extensions.Delegation
	if d == nil {
		t.Fatal("delegation extension not created")
	}
	if d.Depth() != 1 {
		t.Fatalf("depth = %d, want 1", d.Depth())
	}
	hop := d.Chain()[0]
	if hop.Audience != "github-tool" {
		t.Errorf("audience = %q", hop.Audience)
	}
	if hop.Strategy != "token-exchange" {
		t.Errorf("strategy = %q", hop.Strategy)
	}
	if !hop.FromCache {
		t.Errorf("from-cache not propagated")
	}
	if !reflect.DeepEqual(hop.Scopes, []string{"openid", "github-tool-aud"}) {
		t.Errorf("scopes = %v", hop.Scopes)
	}
	if hop.Timestamp.IsZero() {
		t.Errorf("timestamp should be stamped")
	}
}

func TestRecordDelegationHop_SubjectFromIdentityWhenPresent(t *testing.T) {
	pctx := &pipeline.Context{Identity: stubIdentity{subject: "alice"}}
	// A bearer with a different subject is present but MUST be ignored when a
	// validated Identity exists — Identity is the trusted source.
	other := signSubjectToken(t, "someone-else")
	recordDelegationHop(pctx, "Bearer "+other, &auth.OutboundResult{TargetAudience: "tool", RequestedScopes: ""})
	if got := pctx.Extensions.Delegation.Chain()[0].SubjectID; got != "alice" {
		t.Fatalf("subject = %q, want alice", got)
	}
	// Origin/Actor derive from the hop subject.
	if pctx.Extensions.Delegation.Origin != "alice" || pctx.Extensions.Delegation.Actor != "alice" {
		t.Fatalf("origin/actor not derived: origin=%q actor=%q",
			pctx.Extensions.Delegation.Origin, pctx.Extensions.Delegation.Actor)
	}
}

// TestRecordDelegationHop_SubjectFromBearerWhenNoIdentity covers the outbound
// leg: no validated Identity, but the incoming bearer (the RFC 8693
// subject_token) names the delegated caller. We decode its `sub` best-effort so
// the delegation chain — and the OPA-synthesized outbound input.identity —
// carries the real subject instead of empty.
func TestRecordDelegationHop_SubjectFromBearerWhenNoIdentity(t *testing.T) {
	pctx := &pipeline.Context{} // Identity nil
	token := signSubjectToken(t, "dev-user")
	recordDelegationHop(pctx, "Bearer "+token, &auth.OutboundResult{
		TargetAudience:  "github-tool",
		RequestedScopes: "openid agent-team1-github-tool-aud",
	})

	d := pctx.Extensions.Delegation
	if got := d.Chain()[0].SubjectID; got != "dev-user" {
		t.Fatalf("subject = %q, want dev-user", got)
	}
	if d.Origin != "dev-user" || d.Actor != "dev-user" {
		t.Fatalf("origin/actor not derived from bearer: origin=%q actor=%q", d.Origin, d.Actor)
	}
}

// TestRecordDelegationHop_EmptySubjectOnMalformedBearer confirms the fallback
// safe-fails to empty rather than panicking when no usable bearer is present.
func TestRecordDelegationHop_EmptySubjectOnMalformedBearer(t *testing.T) {
	pctx := &pipeline.Context{}
	recordDelegationHop(pctx, "Bearer not-a-jwt", &auth.OutboundResult{TargetAudience: "tool"})
	if got := pctx.Extensions.Delegation.Chain()[0].SubjectID; got != "" {
		t.Fatalf("subject = %q, want empty on malformed bearer", got)
	}
}

// TestSubjectFromToken exercises the unverified decode helper directly.
func TestSubjectFromToken(t *testing.T) {
	if got := subjectFromToken(""); got != "" {
		t.Errorf("empty header: got %q", got)
	}
	if got := subjectFromToken("Bearer "); got != "" {
		t.Errorf("empty bearer: got %q", got)
	}
	if got := subjectFromToken("Bearer garbage"); got != "" {
		t.Errorf("malformed jwt: got %q", got)
	}
	token := signSubjectToken(t, "dev-user")
	if got := subjectFromToken("Bearer " + token); got != "dev-user" {
		t.Errorf("valid jwt: got %q, want dev-user", got)
	}
}

// signSubjectToken mints a signed JWT carrying only the given subject. The
// signature is real (jwx rejects alg=none), but subjectFromToken decodes it
// WITHOUT verification, so the key is throwaway.
func signSubjectToken(t *testing.T, subject string) string {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privJWK, err := jwk.FromRaw(privKey)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.NewBuilder().Subject(subject).Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, privJWK))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

// stubIdentity is a minimal pipeline.Identity for the subject-present path.
type stubIdentity struct{ subject string }

func (s stubIdentity) Subject() string  { return s.subject }
func (s stubIdentity) ClientID() string { return "" }
func (s stubIdentity) Scopes() []string { return nil }
