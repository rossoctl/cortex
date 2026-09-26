package spiffe

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// fakeJWTSVIDFetcher is a hand-rolled fake that satisfies the package
// level jwtSVIDFetcher interface. Mirrors fakeX509SVIDFetcher in
// workload_x509_test.go: go-spiffe's fake workload API lives under
// internal/test and is not importable from outside the SDK, so we mock
// the one method workloadJWT actually consumes instead.
type fakeJWTSVIDFetcher struct {
	svid    *jwtsvid.SVID
	err     error
	gotAuds []string // captured for assertions
}

func (f *fakeJWTSVIDFetcher) FetchJWTSVID(_ context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error) {
	f.gotAuds = append([]string{params.Audience}, params.ExtraAudiences...)
	if f.err != nil {
		return nil, f.err
	}
	return f.svid, nil
}

// makeJWTSVID builds a *jwtsvid.SVID with a real, parseable token whose
// Marshal() returns the same string we put in. The SVID's token field
// is unexported, so we have to round-trip through ParseInsecure rather
// than constructing a literal.
func makeJWTSVID(t *testing.T, audience string) *jwtsvid.SVID {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	privJWK, err := jwk.FromRaw(priv)
	if err != nil {
		t.Fatalf("jwk.FromRaw: %v", err)
	}
	if err := privJWK.Set(jwk.KeyIDKey, "test-key-1"); err != nil {
		t.Fatalf("jwk.Set kid: %v", err)
	}
	if err := privJWK.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("jwk.Set alg: %v", err)
	}

	builder := jwt.NewBuilder().
		Subject("spiffe://example.org/workload").
		Audience([]string{audience}).
		Expiration(time.Now().Add(time.Hour)).
		IssuedAt(time.Now())
	tok, err := builder.Build()
	if err != nil {
		t.Fatalf("jwt.Build: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, privJWK))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}

	svid, err := jwtsvid.ParseInsecure(string(signed), []string{audience})
	if err != nil {
		t.Fatalf("jwtsvid.ParseInsecure: %v", err)
	}
	return svid
}

func TestWorkloadJWT_FetchToken(t *testing.T) {
	const audience = "https://keycloak/realms/test"
	svid := makeJWTSVID(t, audience)

	fake := &fakeJWTSVIDFetcher{svid: svid}
	src := &workloadJWT{sdk: fake, audience: audience}

	token, err := src.FetchToken(context.Background())
	if err != nil {
		t.Fatalf("FetchToken: %v", err)
	}
	if token == "" {
		t.Fatal("FetchToken returned empty token")
	}
	if token != svid.Marshal() {
		t.Fatalf("FetchToken returned %q, want %q", token, svid.Marshal())
	}
	if len(fake.gotAuds) == 0 || fake.gotAuds[0] != audience {
		t.Fatalf("FetchJWTSVID called with audience %v, want %q", fake.gotAuds, audience)
	}
}

func TestWorkloadJWT_FetchToken_FetcherError(t *testing.T) {
	const audience = "https://keycloak/realms/test"
	fake := &fakeJWTSVIDFetcher{err: errors.New("workload api unavailable")}
	src := &workloadJWT{sdk: fake, audience: audience}

	_, err := src.FetchToken(context.Background())
	if err == nil {
		t.Fatal("FetchToken returned nil error when fetcher errored")
	}
	msg := err.Error()
	if !strings.Contains(msg, "workloadJWT:") {
		t.Errorf("FetchToken error %q does not carry workloadJWT prefix", msg)
	}
	if !strings.Contains(msg, audience) {
		t.Errorf("FetchToken error %q does not mention audience %q", msg, audience)
	}
}
