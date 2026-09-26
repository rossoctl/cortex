package praxis

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"gopkg.in/yaml.v3"
)

// jwtPlugin builds a jwt-validation plugin entry with the given config.
func jwtPlugin(t *testing.T, cfg map[string]any) config.PluginEntry {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal plugin config: %v", err)
	}
	return config.PluginEntry{Name: "jwt-validation", Config: raw}
}

// keycloakJWT is the shape the weather-service example uses: public issuer,
// internal Keycloak URL + realm for JWKS derivation, explicit audience.
func keycloakJWT(t *testing.T) config.PluginEntry {
	t.Helper()
	return jwtPlugin(t, map[string]any{
		"issuer":         "http://keycloak.localtest.me:8080/realms/rossoctl",
		"keycloak_url":   "http://keycloak.localtest.me:8080/",
		"keycloak_realm": "rossoctl",
		"audience":       "spiffe://localtest.me/ns/team1/sa/weather-service",
	})
}

func TestBuildPolicy_NilConfig(t *testing.T) {
	if _, err := BuildPolicy(nil, nil); err == nil {
		t.Fatal("expected an error for a nil config")
	}
}

// No jwt-validation in the pipeline means nothing for the policy engine to
// enforce, so no document should be produced — writing one, and pointing a
// policy filter at it, would add a filter that enforces nothing.
func TestBuildPolicy_NoJWTPlugin_NoDocument(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, nil), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if res.Document != nil {
		t.Errorf("expected no policy document, got %+v", res.Document)
	}
	if len(res.Enforced) != 0 {
		t.Errorf("expected nothing enforced, got %v", res.Enforced)
	}
}

func TestBuildPolicy_JWTValidation(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if res.Document == nil {
		t.Fatal("expected a policy document")
	}
	if len(res.Document.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(res.Document.Plugins))
	}
	p := res.Document.Plugins[0]
	if p.Kind != policyPluginKindJWT {
		t.Errorf("kind = %q, want %q", p.Kind, policyPluginKindJWT)
	}
	if len(p.Hooks) != 1 || p.Hooks[0] != policyHookIdentity {
		t.Errorf("hooks = %v, want [%s]", p.Hooks, policyHookIdentity)
	}
	// Fail-closed is the whole point: a plugin that ignores its own errors
	// would let unvalidated requests through.
	if p.OnError != policyOnErrorFail {
		t.Errorf("on_error = %q, want %q", p.OnError, policyOnErrorFail)
	}
	// sequential is the only band that can both block and modify.
	if p.Mode != policyModeSequential {
		t.Errorf("mode = %q, want %q", p.Mode, policyModeSequential)
	}

	jc, ok := p.Config.(JWTIdentityConfig)
	if !ok {
		t.Fatalf("config has type %T", p.Config)
	}
	ti := jc.TrustedIssuers[0]
	if ti.Issuer != "http://keycloak.localtest.me:8080/realms/rossoctl" {
		t.Errorf("issuer = %q", ti.Issuer)
	}
	if len(ti.Audiences) != 1 || ti.Audiences[0] != "spiffe://localtest.me/ns/team1/sa/weather-service" {
		t.Errorf("audiences = %v", ti.Audiences)
	}
	// JWKS must be derived from keycloak_url + keycloak_realm, matching
	// jwt-validation's own derivation.
	want := "http://keycloak.localtest.me:8080/realms/rossoctl/protocol/openid-connect/certs"
	if ti.DecodingKey.URL != want {
		t.Errorf("jwks url = %q, want %q", ti.DecodingKey.URL, want)
	}
	if ti.DecodingKey.Kind != "jwks_url" {
		t.Errorf("decoding_key kind = %q, want jwks_url", ti.DecodingKey.Kind)
	}
	if !containsSubstring(res.Enforced, "jwt-validation") {
		t.Errorf("Enforced = %v, want jwt-validation", res.Enforced)
	}
}

// The policy engine rejects a plaintext http:// JWKS URL unless insecure_http
// is set, so a local config that works under AuthBridge would otherwise fail
// Praxis startup. The flag must be set AND the weakening reported.
func TestBuildPolicy_PlaintextJWKS_SetsInsecureAndWarns(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	if !jc.TrustedIssuers[0].DecodingKey.InsecureHTTP {
		t.Error("expected insecure_http for a plaintext http:// JWKS URL")
	}
	if !containsSubstring(res.Warnings, "insecure_http") {
		t.Errorf("expected a warning about plaintext JWKS, got %v", res.Warnings)
	}
}

func TestBuildPolicy_HTTPSJWKS_NoInsecureFlag(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":   "https://idp.example.com/realms/r",
			"jwks_url": "https://idp.example.com/realms/r/protocol/openid-connect/certs",
			"audience": "svc",
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	if jc.TrustedIssuers[0].DecodingKey.InsecureHTTP {
		t.Error("https JWKS must not set insecure_http")
	}
	if containsSubstring(res.Warnings, "insecure_http") {
		t.Errorf("https JWKS should not warn about plaintext: %v", res.Warnings)
	}
}

// An explicit jwks_url wins over keycloak_url derivation, matching
// jwt-validation's priority order.
func TestBuildPolicy_ExplicitJWKSWins(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":         "https://public.example.com/realms/r",
			"jwks_url":       "https://internal.svc/jwks",
			"keycloak_url":   "https://other.example.com",
			"keycloak_realm": "r",
			"audience":       "svc",
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	if got := jc.TrustedIssuers[0].DecodingKey.URL; got != "https://internal.svc/jwks" {
		t.Errorf("jwks url = %q, want the explicit one", got)
	}
}

// Falling back to the issuer is jwt-validation's third derivation step.
func TestBuildPolicy_JWKSFromIssuer(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":   "https://idp.example.com/realms/r",
			"audience": "svc",
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	want := "https://idp.example.com/realms/r/protocol/openid-connect/certs"
	if got := jc.TrustedIssuers[0].DecodingKey.URL; got != want {
		t.Errorf("jwks url = %q, want %q", got, want)
	}
}

// allowed_audiences and audience are unioned with OR semantics, deduplicated.
func TestBuildPolicy_AudienceUnion(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":            "https://idp.example.com/realms/r",
			"audience":          "primary",
			"allowed_audiences": []string{"extra-1", "extra-2", "primary"},
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	got := jc.TrustedIssuers[0].Audiences
	want := []string{"extra-1", "extra-2", "primary"}
	if len(got) != len(want) {
		t.Fatalf("audiences = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audiences = %v, want %v", got, want)
			break
		}
	}
}

// An audience_file is read at runtime by AuthBridge and cannot be resolved
// here. Emitting the plugin anyway is FAIL-OPEN: Audiences is omitempty, so an
// empty slice omits the key and the engine accepts any token from the issuer.
// That must be an error, not a warning — a warning still ships the policy.
func TestBuildPolicy_AudienceFile_IsError(t *testing.T) {
	_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":        "https://idp.example.com/realms/r",
			"audience_file": "/shared/client-id.txt",
		})}
	}), nil)
	if err == nil {
		t.Fatal("expected an error: audience_file cannot be resolved, so aud would go unvalidated")
	}
	if !strings.Contains(err.Error(), "/shared/client-id.txt") {
		t.Errorf("error should name the file: %v", err)
	}
}

// audience_mode: per-host derives the audience per request, which is equally
// unrepresentable in a static trusted_issuers list, so it fails for the same
// fail-open reason.
func TestBuildPolicy_PerHostAudience_IsError(t *testing.T) {
	_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":        "https://idp.example.com/realms/r",
			"audience_mode": "per-host",
		})}
	}), nil)
	if err == nil {
		t.Fatal("expected an error: per-host audience cannot be expressed statically")
	}
	if !strings.Contains(err.Error(), "per-host") {
		t.Errorf("error should mention per-host: %v", err)
	}
}

// No audience of any kind is the third fail-open path.
func TestBuildPolicy_NoAudience_IsError(t *testing.T) {
	_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer": "https://idp.example.com/realms/r",
		})}
	}), nil)
	if err == nil {
		t.Fatal("expected an error when no audience is declared")
	}
}

// writeAudienceFile creates a file holding aud and returns its path.
func writeAudienceFile(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "client-id.txt")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write audience file: %v", err)
	}
	return p
}

// The in-cluster shape: jwt-validation names no literal audience because the
// operator mounts the client ID at /shared/client-id.txt. Pointing the
// converter at that file must produce a policy, not an error.
func TestBuildPolicy_AudienceFromFile(t *testing.T) {
	path := writeAudienceFile(t, "agent-team1-weather-service\n")
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":        "https://idp.example.com/realms/r",
			"audience_file": "/shared/client-id.txt",
		})}
	}), &PolicyOptions{AudienceFile: path})
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if res.Document == nil {
		t.Fatal("expected a policy document when the audience file supplies the audience")
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	auds := jc.TrustedIssuers[0].Audiences
	// Trailing newline must be trimmed, or the aud claim never matches.
	if len(auds) != 1 || auds[0] != "agent-team1-weather-service" {
		t.Errorf("audiences = %v, want [agent-team1-weather-service]", auds)
	}
	// Baking a runtime-read value into a static file is worth stating: rotating
	// the client ID silently invalidates the generated policy.
	if !containsSubstring(res.Warnings, "regenerate") {
		t.Errorf("expected a warning that the value is baked in, got %v", res.Warnings)
	}
}

// A file audience unions with an explicit one under the same OR semantics
// jwt-validation uses, rather than either replacing the other.
func TestBuildPolicy_AudienceFileUnionsWithLiteral(t *testing.T) {
	path := writeAudienceFile(t, "from-file")
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":   "https://idp.example.com/realms/r",
			"audience": "from-config",
		})}
	}), &PolicyOptions{AudienceFile: path})
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	auds := jc.TrustedIssuers[0].Audiences
	if len(auds) != 2 {
		t.Fatalf("audiences = %v, want both the config and file values", auds)
	}
	found := map[string]bool{}
	for _, a := range auds {
		found[a] = true
	}
	if !found["from-config"] || !found["from-file"] {
		t.Errorf("audiences = %v, want both from-config and from-file", auds)
	}
}

// A duplicate between file and config must dedupe, not emit the value twice.
func TestBuildPolicy_AudienceFileDedupes(t *testing.T) {
	path := writeAudienceFile(t, "same")
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":   "https://idp.example.com/realms/r",
			"audience": "same",
		})}
	}), &PolicyOptions{AudienceFile: path})
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	if auds := jc.TrustedIssuers[0].Audiences; len(auds) != 1 {
		t.Errorf("audiences = %v, want one deduplicated entry", auds)
	}
}

// An explicitly named audience file that cannot be read is an error, not a
// silent fallback: falling through would emit a policy with no audience, which
// accepts any token from the issuer.
func TestBuildPolicy_AudienceFileUnreadable_IsError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents *string
	}{
		{name: "missing file"},
		{name: "empty file", contents: strPtr("")},
		{name: "whitespace only", contents: strPtr("   \n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "absent.txt")
			if tc.contents != nil {
				path = writeAudienceFile(t, *tc.contents)
			}
			_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
				c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
					"issuer":        "https://idp.example.com/realms/r",
					"audience_file": "/shared/client-id.txt",
				})}
			}), &PolicyOptions{AudienceFile: path})
			if err == nil {
				t.Error("expected an error: an unreadable audience file must not fall through " +
					"to an audience-less policy")
			}
		})
	}
}

// A whitespace-only file trims to empty and must never become an audience
// entry. ReadCredentialFile rejects a zero-byte file outright, but a file of
// only whitespace is non-zero on disk and trims away — so this guards the trim
// path rather than the size check. Uses a plugin with a literal audience so the
// conversion still succeeds: the assertion is that the blank value is not
// silently added alongside it.
func TestBuildPolicy_AudienceFileWhitespaceNotAnAudience(t *testing.T) {
	path := writeAudienceFile(t, "\n\t \n")
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":   "https://idp.example.com/realms/r",
			"audience": "real-aud",
		})}
	}), &PolicyOptions{AudienceFile: path})
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	auds := res.Document.Plugins[0].Config.(JWTIdentityConfig).TrustedIssuers[0].Audiences
	if len(auds) != 1 || auds[0] != "real-aud" {
		t.Errorf("audiences = %v, want only [real-aud]: a whitespace-only file must contribute "+
			"nothing, and an empty entry would disable aud matching for that value", auds)
	}
	for _, a := range auds {
		if strings.TrimSpace(a) == "" {
			t.Errorf("audiences contains a blank entry: %q", a)
		}
	}
}

// No audience file configured keeps the previous behavior: the literal audience
// alone is enough.
func TestBuildPolicy_NoAudienceFile_LiteralStillWorks(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), &PolicyOptions{})
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if res.Document == nil {
		t.Fatal("expected a policy document")
	}
	if containsSubstring(res.Warnings, "regenerate") {
		t.Errorf("no audience file was read, so no baked-in warning belongs: %v", res.Warnings)
	}
}

// The error for an unresolvable audience should point at the flag that fixes
// it, naming the plugin's own audience_file path.
func TestBuildPolicy_AudienceError_SuggestsAudienceFile(t *testing.T) {
	_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":        "https://idp.example.com/realms/r",
			"audience_file": "/shared/client-id.txt",
		})}
	}), nil)
	if err == nil {
		t.Fatal("expected an error when no audience can be resolved")
	}
	if !strings.Contains(err.Error(), "--audience-file") {
		t.Errorf("error should point at the flag that fixes it: %v", err)
	}
	if !strings.Contains(err.Error(), "/shared/client-id.txt") {
		t.Errorf("error should name the plugin's audience_file: %v", err)
	}
}

func strPtr(s string) *string { return &s }

// Guard the fail-open mechanism directly: if Audiences is ever populated empty,
// omitempty drops the key and the engine stops validating aud. Any policy this
// converter emits must carry at least one audience.
func TestBuildPolicy_EmittedPolicyAlwaysHasAudiences(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	for _, p := range res.Document.Plugins {
		jc, ok := p.Config.(JWTIdentityConfig)
		if !ok {
			continue
		}
		for i, ti := range jc.TrustedIssuers {
			if len(ti.Audiences) == 0 {
				t.Errorf("plugin %q trusted_issuers[%d] has no audiences; omitempty would drop "+
					"the key and the engine would accept any token from the issuer", p.Name, i)
			}
		}
	}

	// And confirm the rendered YAML actually carries the key.
	out, err := RenderPolicyResult(res, "/tmp/c.yaml")
	if err != nil {
		t.Fatalf("RenderPolicyResult: %v", err)
	}
	if !strings.Contains(string(out), "audiences:") {
		t.Errorf("rendered policy must carry an audiences key:\n%s", out)
	}
}

// AuthBridge's verifier does not restrict algorithms (jwt.WithKeySet accepts
// whatever the JWKS key advertises), but the policy engine requires an explicit
// list. Defaulting to RS256 alone would reject every token from an ES256 or
// RS512 realm — a total inbound outage. The default must cover the asymmetric
// families and be reported.
func TestBuildPolicy_DefaultAlgorithms(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	algs := jc.TrustedIssuers[0].Algorithms
	for _, want := range []string{"RS256", "RS512", "ES256", "PS256", "EdDSA"} {
		found := false
		for _, a := range algs {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("default algorithms %v missing %q", algs, want)
		}
	}
	// HS* is symmetric and cannot be a JWKS verification key here; including it
	// alongside an asymmetric issuer invites algorithm confusion.
	for _, a := range algs {
		if strings.HasPrefix(a, "HS") {
			t.Errorf("default algorithms must not include symmetric %q: %v", a, algs)
		}
	}
	if !containsSubstring(res.Warnings, "algorithms") {
		t.Errorf("defaulting the algorithm list must be reported, got %v", res.Warnings)
	}
}

// An explicit algorithms list on the plugin must win over the default, so the
// generated policy tracks the realm rather than this converter's guess.
func TestBuildPolicy_ExplicitAlgorithmsWin(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":     "https://idp.example.com/realms/r",
			"audience":   "svc",
			"algorithms": []string{"ES384"},
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	jc := res.Document.Plugins[0].Config.(JWTIdentityConfig)
	algs := jc.TrustedIssuers[0].Algorithms
	if len(algs) != 1 || algs[0] != "ES384" {
		t.Errorf("algorithms = %v, want [ES384]", algs)
	}
	if containsSubstring(res.Warnings, "names no signing algorithms") {
		t.Errorf("an explicit list must not warn about defaulting: %v", res.Warnings)
	}
}

// A malformed JWKS URL must fail rather than being written into the policy: it
// would be an undefined key source that reads as secure, since insecure_http
// stays false and no warning fires.
func TestBuildPolicy_MalformedJWKS_IsError(t *testing.T) {
	for _, tc := range []struct{ name, keycloakURL string }{
		{"no scheme", "keycloak.example.com:8080"},
		{"unsupported scheme", "ftp://keycloak.example.com"},
		{"scheme only", "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
				c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
					"issuer":         "https://idp.example.com/realms/r",
					"audience":       "svc",
					"keycloak_url":   tc.keycloakURL,
					"keycloak_realm": "r",
				})}
			}), nil)
			if err == nil {
				t.Errorf("expected an error for keycloak_url %q", tc.keycloakURL)
			}
		})
	}
}

// bypass_paths has no identity-plugin equivalent, so those paths now require a
// token. Health probes flipping from open to 401 must be surfaced.
func TestBuildPolicy_BypassPaths_Warns(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"issuer":       "https://idp.example.com/realms/r",
			"audience":     "svc",
			"bypass_paths": []string{"/healthz", "/metrics"},
		})}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if !containsSubstring(res.Warnings, "/healthz") {
		t.Errorf("expected a bypass_paths warning naming the paths, got %v", res.Warnings)
	}
}

// Generating a policy that cannot verify anything would be worse than failing:
// it would look like enforcement while accepting every token.
func TestBuildPolicy_MissingIssuer_IsError(t *testing.T) {
	_, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{jwtPlugin(t, map[string]any{
			"audience": "svc",
		})}
	}), nil)
	if err == nil {
		t.Fatal("expected an error when jwt-validation has no issuer")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("error should mention issuer: %v", err)
	}
}

// on_error: observe is shadow mode — AuthBridge evaluates jwt-validation but
// converts its rejection into a pass-through, so unauthenticated requests reach
// the app on purpose. The policy engine has no shadow equivalent, so the
// generated policy enforces for real and 401s that traffic. The flip is toward
// MORE enforcement (so not a bypass, hence a warning not an error), but it can
// cause an outage for an operator mid-canary and must be stated.
func TestBuildPolicy_ObservePlugin_WarnsAboutEnforcementFlip(t *testing.T) {
	e := keycloakJWT(t)
	e.OnError = pipeline.ErrorPolicyObserve
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{e}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	// Still translated: observe means "evaluate but don't block", so the plugin
	// is active and belongs in the policy — just with different consequences.
	if res.Document == nil {
		t.Fatal("expected a policy document: observe still evaluates the plugin")
	}
	if !containsSubstring(res.Warnings, "shadow mode") {
		t.Errorf("expected a warning naming shadow mode, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "401") {
		t.Errorf("the warning should say what changes for live traffic, got %v", res.Warnings)
	}
	// The emitted plugin is fail-closed regardless, which is the whole point of
	// the warning.
	if res.Document.Plugins[0].OnError != policyOnErrorFail {
		t.Errorf("on_error = %q, want %q", res.Document.Plugins[0].OnError, policyOnErrorFail)
	}
}

// enforce (and the empty default) is the ordinary case and must not warn.
func TestBuildPolicy_EnforcePlugin_NoObserveWarning(t *testing.T) {
	for _, policy := range []pipeline.ErrorPolicy{"", pipeline.ErrorPolicyEnforce} {
		e := keycloakJWT(t)
		e.OnError = policy
		res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
			c.Pipeline.Inbound.Plugins = []config.PluginEntry{e}
		}), nil)
		if err != nil {
			t.Fatalf("BuildPolicy(on_error=%q): %v", policy, err)
		}
		if containsSubstring(res.Warnings, "shadow mode") {
			t.Errorf("on_error=%q must not warn about shadow mode: %v", policy, res.Warnings)
		}
	}
}

// on_error: off means AuthBridge drops the plugin, so no policy is generated
// for it — otherwise the Praxis proxy would enforce what AuthBridge did not.
func TestBuildPolicy_DisabledPlugin_NoDocument(t *testing.T) {
	res, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		e := keycloakJWT(t)
		e.OnError = pipeline.ErrorPolicyOff
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{e}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	if res.Document != nil {
		t.Error("a plugin disabled with on_error: off must not produce a policy")
	}
}

// With a policy document, the inbound chain must carry the `policy` filter
// instead of the inert UNMAPPED marker, and jwt-validation must no longer be
// reported unmapped.
func TestConvertWithPolicy_EmitsPolicyFilter(t *testing.T) {
	cfg := proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	})
	res, pol, err := ConvertWithPolicy(cfg, "/tmp/praxis-policy.yaml", nil)
	if err != nil {
		t.Fatalf("ConvertWithPolicy: %v", err)
	}
	if pol.Document == nil {
		t.Fatal("expected a policy document")
	}
	if containsSubstring(res.Unmapped, "jwt-validation") {
		t.Errorf("jwt-validation is enforced by the policy and must not be unmapped: %v", res.Unmapped)
	}

	var found *Filter
	for i, f := range res.Config.FilterChains[0].Filters {
		if f.Type == "policy" {
			found = &res.Config.FilterChains[0].Filters[i]
		}
	}
	if found == nil {
		t.Fatal("expected a policy filter in the inbound chain")
	}
	var gotPath any
	var gotMeta any
	for _, fl := range found.Fields {
		switch fl.Key {
		case "config_path":
			gotPath = fl.Value
		case "require_protocol_metadata":
			gotMeta = fl.Value
		}
	}
	if gotPath != "/tmp/praxis-policy.yaml" {
		t.Errorf("config_path = %v, want the policy path", gotPath)
	}
	// True (the upstream default) would reject every request for missing
	// classifier metadata rather than judging it on its token.
	if gotMeta != false {
		t.Errorf("require_protocol_metadata = %v, want false", gotMeta)
	}
}

// Without a policy path, the auth plugin stays unmapped and no policy filter is
// emitted — a default-feature Praxis build must still get a loadable config.
func TestConvertWithoutPolicy_NoPolicyFilter(t *testing.T) {
	res, err := Convert(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, f := range res.Config.FilterChains[0].Filters {
		if f.Type == "policy" {
			t.Error("no policy filter should be emitted without a policy path")
		}
	}
	if !containsSubstring(res.Unmapped, "jwt-validation") {
		t.Errorf("expected jwt-validation unmapped, got %v", res.Unmapped)
	}
}

// A pipeline with no enforceable plugin must not get a policy filter, since it
// would point at a file the caller never writes and Praxis would fail to start.
func TestConvertWithPolicy_NoEnforceable_NoFilter(t *testing.T) {
	res, pol, err := ConvertWithPolicy(proxySidecar(t, nil), "/tmp/praxis-policy.yaml", nil)
	if err != nil {
		t.Fatalf("ConvertWithPolicy: %v", err)
	}
	if pol.Document != nil {
		t.Error("expected no policy document")
	}
	for _, ch := range res.Config.FilterChains {
		for _, f := range ch.Filters {
			if f.Type == "policy" {
				t.Error("no policy filter should be emitted when nothing is enforceable")
			}
		}
	}
}

// Two jwt-validation entries in one stage still yield a single policy filter:
// one document carries both, so a second entry would re-run the same engine.
func TestConvertWithPolicy_SinglePolicyFilterPerChain(t *testing.T) {
	res, _, err := ConvertWithPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t), keycloakJWT(t)}
	}), "/tmp/praxis-policy.yaml", nil)
	if err != nil {
		t.Fatalf("ConvertWithPolicy: %v", err)
	}
	n := 0
	for _, f := range res.Config.FilterChains[0].Filters {
		if f.Type == "policy" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("policy filters = %d, want 1", n)
	}
}

// BuildPolicy reads only the INBOUND pipeline, so an outbound jwt-validation
// entry must NOT produce a `policy` filter on the outbound chain: that filter
// would point at a document describing inbound identity, enforcing the wrong
// stage's rules on egress while reporting the plugin as translated.
func TestConvertWithPolicy_OutboundJWTDoesNotEmitPolicyFilter(t *testing.T) {
	cfg := proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
		// Same plugin name, wrong direction.
		c.Pipeline.Outbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	})
	res, pol, err := ConvertWithPolicy(cfg, "/tmp/praxis-policy.yaml", nil)
	if err != nil {
		t.Fatalf("ConvertWithPolicy: %v", err)
	}
	if pol.Document == nil {
		t.Fatal("expected a policy document from the inbound plugin")
	}

	byChain := map[string]int{}
	for _, ch := range res.Config.FilterChains {
		for _, f := range ch.Filters {
			if f.Type == "policy" {
				byChain[ch.Name]++
			}
		}
	}
	if byChain[ChainInbound] != 1 {
		t.Errorf("inbound chain policy filters = %d, want 1", byChain[ChainInbound])
	}
	if byChain[ChainOutbound] != 0 {
		t.Errorf("outbound chain must carry no policy filter (the document is inbound-only), got %d",
			byChain[ChainOutbound])
	}
	// The outbound entry is not enforced by anything, so it must be reported.
	if !containsSubstring(res.Unmapped, "jwt-validation") {
		t.Errorf("an outbound jwt-validation is unenforced and must be reported unmapped: %v",
			res.Unmapped)
	}
}

// A policy document is written but no inbound listener can be generated, so
// nothing loads it. That must be surfaced, or the caller logs
// "wrote Praxis policy, enforces=[jwt-validation]" for enforcement that does
// not exist.
func TestConvertWithPolicy_NoBackend_WarnsPolicyNotEnforced(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ModeProxySidecar,
		Listener: config.ListenerConfig{
			Roles: []string{config.RoleReverse, config.RoleForward},
			// No reverse_proxy_backend.
		},
	}
	cfg.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	config.ApplyPreset(cfg)

	res, pol, err := ConvertWithPolicy(cfg, "/tmp/praxis-policy.yaml", nil)
	if err != nil {
		t.Fatalf("ConvertWithPolicy: %v", err)
	}
	if pol.Document == nil {
		t.Fatal("expected a policy document (the inbound plugin is present)")
	}
	for _, l := range res.Config.Listeners {
		if l.Name == ListenerInbound {
			t.Fatal("expected no inbound listener without a backend")
		}
	}
	if !containsSubstring(res.Warnings, "reverse_proxy_backend") {
		t.Errorf("expected a warning naming the missing field, got %v", res.Warnings)
	}
	if !containsSubstring(res.Warnings, "not enforced") {
		t.Errorf("expected the warning to say inbound plugins are unenforced, got %v", res.Warnings)
	}
}

func TestRenderPolicyResult_ParsesAsYAML(t *testing.T) {
	pol, err := BuildPolicy(proxySidecar(t, func(c *config.Config) {
		c.Pipeline.Inbound.Plugins = []config.PluginEntry{keycloakJWT(t)}
	}), nil)
	if err != nil {
		t.Fatalf("BuildPolicy: %v", err)
	}
	out, err := RenderPolicyResult(pol, "/tmp/praxis-config.yaml")
	if err != nil {
		t.Fatalf("RenderPolicyResult: %v", err)
	}
	var probe struct {
		Plugins []struct {
			Name   string   `yaml:"name"`
			Kind   string   `yaml:"kind"`
			Hooks  []string `yaml:"hooks"`
			Config struct {
				TrustedIssuers []struct {
					Issuer      string   `yaml:"issuer"`
					Audiences   []string `yaml:"audiences"`
					Algorithms  []string `yaml:"algorithms"`
					DecodingKey struct {
						Kind string `yaml:"kind"`
						URL  string `yaml:"url"`
					} `yaml:"decoding_key"`
				} `yaml:"trusted_issuers"`
			} `yaml:"config"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("rendered policy does not parse: %v\n%s", err, out)
	}
	if len(probe.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(probe.Plugins))
	}
	if probe.Plugins[0].Kind != policyPluginKindJWT {
		t.Errorf("kind = %q", probe.Plugins[0].Kind)
	}
	if len(probe.Plugins[0].Config.TrustedIssuers) != 1 {
		t.Fatal("expected one trusted issuer to survive the round trip")
	}
	if probe.Plugins[0].Config.TrustedIssuers[0].DecodingKey.Kind != "jwks_url" {
		t.Error("expected the jwks_url decoding key to survive the round trip")
	}
}

func TestRenderPolicyResult_NilDocument_IsError(t *testing.T) {
	if _, err := RenderPolicyResult(&PolicyResult{}, "x.yaml"); err == nil {
		t.Fatal("expected an error rendering a nil document")
	}
}

// TestGeneratedPolicy_ValidatesWithPraxis runs the real policy-engine Praxis
// binary against the generated pair. This is what actually pins the goal: the
// policy engine parses the document at filter-construction time and fails the
// server start on a malformed policy, which no Go-side assertion can stand in
// for.
//
// Requires a Praxis binary built with --features policy-engine. Skipped
// otherwise, and skipped when the available binary is a default build (detected
// by it rejecting the policy filter as unknown).
func TestGeneratedPolicy_ValidatesWithPraxis(t *testing.T) {
	bin := findPraxisBinary(t)
	if bin == "" {
		t.Skip("no praxis binary found; set PRAXIS_BIN to enable this test")
	}
	if !praxisHasPolicyEngine(t, bin) {
		t.Skip("praxis binary lacks the policy-engine feature; rebuild with --features policy-engine")
	}

	cases := []struct {
		name    string
		plugins []config.PluginEntry
	}{
		{
			name:    "keycloak issuer with explicit audience",
			plugins: []config.PluginEntry{keycloakJWT(t)},
		},
		{
			name: "https jwks, multiple audiences",
			plugins: []config.PluginEntry{jwtPlugin(t, map[string]any{
				"issuer":            "https://idp.example.com/realms/r",
				"jwks_url":          "https://idp.example.com/realms/r/protocol/openid-connect/certs",
				"audience":          "primary",
				"allowed_audiences": []string{"extra"},
			})},
		},
		{
			// Pins that every entry of defaultJWTAlgorithms is a variant the
			// engine's Algorithm enum accepts. An invalid one (ES512, which
			// upstream does NOT accept despite RS512/PS512 being valid) makes
			// the engine reject the whole document at startup.
			name: "default algorithm set is accepted by the engine",
			plugins: []config.PluginEntry{jwtPlugin(t, map[string]any{
				"issuer":   "https://idp.example.com/realms/r",
				"jwks_url": "https://idp.example.com/realms/r/protocol/openid-connect/certs",
				"audience": "svc",
			})},
		},
		{
			name: "bypass paths declared",
			plugins: []config.PluginEntry{jwtPlugin(t, map[string]any{
				"issuer":       "https://idp.example.com/realms/r",
				"audience":     "svc",
				"bypass_paths": []string{"/healthz", "/.well-known/*"},
			})},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode: config.ModeProxySidecar,
				Listener: config.ListenerConfig{
					Roles:               []string{config.RoleReverse},
					ReverseProxyBackend: "http://127.0.0.1:8001",
				},
				// Loopback so the admin bind needs no insecure override.
				Stats: config.StatsConfig{StatsAddress: "127.0.0.1:19093"},
			}
			cfg.Pipeline.Inbound.Plugins = tc.plugins
			config.ApplyPreset(cfg)

			dir := t.TempDir()
			policyPath := filepath.Join(dir, "praxis-policy.yaml")
			cfgPath := filepath.Join(dir, "praxis-config.yaml")

			res, pol, err := ConvertWithPolicy(cfg, policyPath, nil)
			if err != nil {
				t.Fatalf("ConvertWithPolicy: %v", err)
			}
			if pol.Document == nil {
				t.Fatal("expected a policy document")
			}
			policyData, err := RenderPolicyResult(pol, cfgPath)
			if err != nil {
				t.Fatalf("RenderPolicyResult: %v", err)
			}
			if err := os.WriteFile(policyPath, policyData, 0o600); err != nil {
				t.Fatalf("write policy: %v", err)
			}
			cfgData, err := RenderResult(res, cfgPath)
			if err != nil {
				t.Fatalf("RenderResult: %v", err)
			}
			if err := os.WriteFile(cfgPath, cfgData, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			out, err := exec.Command(bin, "-t", "-c", cfgPath).CombinedOutput()
			if err != nil {
				t.Errorf("praxis rejected the generated pair: %v\n--- praxis ---\n%s\n--- config ---\n%s\n--- policy ---\n%s",
					err, out, cfgData, policyData)
			}
		})
	}
}

// praxisHasPolicyEngine reports whether the binary was built with the
// policy-engine feature, by checking whether it recognizes the filter name.
func praxisHasPolicyEngine(t *testing.T, bin string) bool {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.yaml")
	// A policy filter pointing at a missing file: a default build reports
	// "unknown filter type", a policy-engine build gets far enough to complain
	// about the unreadable config_path.
	const yamlBody = `listeners:
  - name: l
    address: "127.0.0.1:18099"
    filter_chains: [c]
filter_chains:
  - name: c
    filters:
      - filter: policy
        config_path: /nonexistent/policy.yaml
      - filter: static_response
        status: 200
`
	if err := os.WriteFile(probe, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	out, _ := exec.Command(bin, "-t", "-c", probe).CombinedOutput()
	return !strings.Contains(string(out), "unknown filter type")
}
