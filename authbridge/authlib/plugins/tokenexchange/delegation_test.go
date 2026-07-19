package tokenexchange

import (
	"reflect"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/auth"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
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
	recordDelegationHop(pctx, result)

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
	recordDelegationHop(pctx, &auth.OutboundResult{TargetAudience: "tool", RequestedScopes: ""})
	if got := pctx.Extensions.Delegation.Chain()[0].SubjectID; got != "alice" {
		t.Fatalf("subject = %q, want alice", got)
	}
	// Origin/Actor derive from the hop subject.
	if pctx.Extensions.Delegation.Origin != "alice" || pctx.Extensions.Delegation.Actor != "alice" {
		t.Fatalf("origin/actor not derived: origin=%q actor=%q",
			pctx.Extensions.Delegation.Origin, pctx.Extensions.Delegation.Actor)
	}
}

// stubIdentity is a minimal pipeline.Identity for the subject-present path.
type stubIdentity struct{ subject string }

func (s stubIdentity) Subject() string  { return s.subject }
func (s stubIdentity) ClientID() string { return "" }
func (s stubIdentity) Scopes() []string { return nil }
