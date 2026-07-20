package tokenexchange

import (
	"errors"
	"strings"

	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange/exchange"
	fwspiffe "github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
)

// keycloakProvider derives endpoints and builds client auth from
// Keycloak's conventions.
//
// Config example:
//
//	provider: keycloak
//	provider_url: https://keycloak.example.com
//	provider_realm: my-realm
//	identity:
//	  type: client-secret   # or spiffe
type keycloakProvider struct{}

func (keycloakProvider) Name() string { return KeycloakProvider }

func (keycloakProvider) TokenEndpoint(providerURL, providerRealm string) string {
	base := strings.TrimRight(providerURL, "/")
	if base == "" || providerRealm == "" {
		return ""
	}
	return base + "/realms/" + providerRealm + "/protocol/openid-connect/token"
}

func (keycloakProvider) DefaultAssertionType() string { return DefaultAssertion }

func (keycloakProvider) SupportedIdentityTypes() []string {
	return []string{ClientSecretIdentity, SpiffeIdentity}
}

func (keycloakProvider) BuildClientAuth(id IdentityConfig, jwtSrc fwspiffe.JWTSource) (exchange.ClientAuth, error) {
	switch id.Type {
	case SpiffeIdentity:
		if jwtSrc == nil {
			return nil, errors.New("spiffe identity requires a SPIFFE provider to be injected")
		}
		assertionType := id.AssertionType
		if assertionType == "" {
			assertionType = DefaultAssertion
		}
		urn, ok := AssertionTypeURN[assertionType]
		if !ok {
			return nil, errors.New("keycloak: unsupported assertion_type " + assertionType)
		}
		return &exchange.JWTAssertionAuth{
			ClientID:      id.ClientID,
			AssertionType: urn,
			TokenSource:   jwtSrc.FetchToken,
		}, nil
	case ClientSecretIdentity:
		return &exchange.ClientSecretAuth{
			ClientID:     id.ClientID,
			ClientSecret: id.ClientSecret,
		}, nil
	default:
		return nil, errors.New("keycloak: unsupported identity.type " + id.Type)
	}
}

func init() { RegisterProvider(keycloakProvider{}) }
