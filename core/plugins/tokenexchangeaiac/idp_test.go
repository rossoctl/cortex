package tokenexchangeaiac

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want outcome
	}{
		// The four answers Keycloak 26.5 gave in the live probe.
		{"no such client", &idpError{400, "invalid_client", "Audience not found"}, outcomeUnknownAudience},
		{"not granted", &idpError{400, "invalid_request", "Requested audience not available: other-tool"}, outcomeNotGranted},
		{"target disabled", &idpError{400, "invalid_client", "Client disabled"}, outcomeTargetDisabled},
		{"invalid subject", &idpError{400, "invalid_request", "Invalid token"}, outcomeSubjectInvalid},

		// Same codes, other meanings: none may become a passthrough.
		{"disabled, wrong status", &idpError{500, "invalid_client", "Client disabled"}, outcomeFailed},
		{"bad credentials", &idpError{401, "invalid_client", "Invalid client or Invalid client credentials"}, outcomeFailed},
		{"subject not ours", &idpError{400, "invalid_request", "Client is not within the token audience"}, outcomeFailed},
		{"right text, wrong status", &idpError{500, "invalid_client", "Audience not found"}, outcomeFailed},
		{"invalid subject, wrong code", &idpError{400, "invalid_client", "Invalid token"}, outcomeFailed},
		{"legacy exchange", &idpError{403, "access_denied", "Client not allowed to exchange"}, outcomeFailed},
		{"server error", &idpError{503, "", ""}, outcomeFailed},
		{"transport", errors.New("dial tcp: connection refused"), outcomeFailed},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

func jwtWithAud(t *testing.T, aud any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"aud": aud, "sub": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestAudienceContains(t *testing.T) {
	if err := audienceContains(jwtWithAud(t, "github-tool"), "github-tool"); err != nil {
		t.Errorf("string aud: %v", err)
	}
	if err := audienceContains(jwtWithAud(t, []string{"account", "github-tool"}), "github-tool"); err != nil {
		t.Errorf("array aud: %v", err)
	}
	if err := audienceContains(jwtWithAud(t, []string{"account", "other"}), "github-tool"); err == nil {
		t.Error("aud without the requested audience was accepted")
	}
	if err := audienceContains(jwtWithAud(t, nil), "github-tool"); err == nil {
		t.Error("token with no aud was accepted")
	}
	if err := audienceContains("opaque-token", "github-tool"); err == nil {
		t.Error("opaque token was accepted")
	}
}
