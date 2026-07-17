package tokenexchange

import (
	"fmt"
	"sync"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/exchange"
	fwspiffe "github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
)

// Identity type constants.
const (
	ClientSecretIdentity = "client-secret"
	SpiffeIdentity       = "spiffe"
)

// Assertion type short names (keys into AssertionTypeURN).
const (
	JWTSpiffeAssertion = "jwt-spiffe"
	JWTBearerAssertion = "jwt-bearer"
)

// Outbound default policy constants.
const (
	PassthroughPolicy = "passthrough"
	ExchangePolicy    = "exchange"
)

// Provider name constants.
const (
	KeycloakProvider = "keycloak"
	GenericProvider  = "generic"
)

// Default values.
const (
	DefaultAssertion = JWTSpiffeAssertion
	DefaultProvider  = KeycloakProvider
)

// IdPProvider defines the contract for an Identity Provider backend.
// Each IdP (Keycloak, Entra ID, Okta, etc.) implements this interface
// to provide endpoint derivation and client authentication from its
// conventions.
//
// Adding a new IdP:
//   1. Create a new file (e.g. provider_okta.go)
//   2. Implement IdPProvider
//   3. Call RegisterProvider() in an init() function
//
// The init() auto-registration pattern means any provider file that
// is compiled into the binary is automatically available — no central
// list to maintain.
type IdPProvider interface {
	// Name returns the provider identifier used in config
	// (e.g. "keycloak", "entra-id", "okta").
	Name() string

	// TokenEndpoint derives the OAuth token endpoint URL from the
	// provider base URL and realm/tenant. Returns "" if the inputs
	// are insufficient (caller must supply explicit token_url).
	TokenEndpoint(providerURL, providerRealm string) string

	// DefaultAssertionType returns the default client_assertion_type
	// URN for this provider when using SPIFFE/JWT identity.
	// E.g. "jwt-spiffe" for Keycloak, "jwt-bearer" for Okta.
	// Returns "" if the provider does not support JWT assertions.
	DefaultAssertionType() string

	// SupportedIdentityTypes returns the identity.type values this
	// provider supports (e.g. ["client-secret", "spiffe"] for
	// Keycloak, ["client-secret", "certificate"] for Entra ID).
	// Used at Configure() to reject unsupported combinations early.
	SupportedIdentityTypes() []string

	// BuildClientAuth constructs the provider-appropriate ClientAuth
	// from the identity config. Each provider owns its auth strategy —
	// Keycloak uses ClientSecretAuth or JWTAssertionAuth(jwt-spiffe),
	// Okta would use JWTAssertionAuth(jwt-bearer), Entra ID would use
	// CertificateAuth (future).
	BuildClientAuth(identity IdentityConfig, jwtSrc fwspiffe.JWTSource) (exchange.ClientAuth, error)
}

// IdentityConfig carries the identity fields a provider needs to
// construct its ClientAuth. Extracted from tokenExchangeIdentity to
// avoid exporting the full plugin config struct.
type IdentityConfig struct {
	Type          string
	ClientID      string
	ClientSecret  string
	AssertionType string
	JWTAudience   string
}

var (
	providersMu sync.RWMutex
	providers   = map[string]IdPProvider{}
)

// RegisterProvider registers an IdP provider. Called from init() in
// each provider's file. Panics on duplicate names.
func RegisterProvider(p IdPProvider) {
	providersMu.Lock()
	defer providersMu.Unlock()
	name := p.Name()
	if _, exists := providers[name]; exists {
		panic(fmt.Sprintf("token-exchange: duplicate IdP provider registration: %q", name))
	}
	providers[name] = p
}

// LookupProvider returns the registered provider for the given name,
// or nil if not found.
func LookupProvider(name string) IdPProvider {
	providersMu.RLock()
	defer providersMu.RUnlock()
	return providers[name]
}

// AssertionTypeURN maps short assertion type names to their full URN
// as used in the RFC 8693 client_assertion_type parameter. Providers
// use this in BuildClientAuth to resolve the configured assertion type
// to the wire-format URN. Also used by validate() to reject unknown values.
var AssertionTypeURN = map[string]string{
	JWTSpiffeAssertion: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
	JWTBearerAssertion: "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
}
