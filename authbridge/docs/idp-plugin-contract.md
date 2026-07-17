# IdP-Agnostic Token Exchange Plugin Contract

> **Status:** Implemented — RHAIENG-5681 / kagenti-extensions#481
>
> This document defines the contract that an Identity Provider (IdP)
> plugin must satisfy for the AuthBridge token exchange pipeline.
> Contributors implementing Entra ID, Okta, or other IdP support should
> use this as their specification.

## Overview

The token exchange plugin (`token-exchange`) implements RFC 8693 token
exchange for outbound requests. The core pipeline is IdP-agnostic:

```text
Request → Route resolver → Token cache → RFC 8693 exchange → Inject token
```

IdP-specific behavior is owned by the `IdPProvider` interface. Each
provider (Keycloak, Entra ID, Okta, etc.) implements this interface
in its own file and self-registers via `init()`.

## IdPProvider Interface

```go
// provider.go
type IdPProvider interface {
    Name() string
    TokenEndpoint(providerURL, providerRealm string) string
    DefaultAssertionType() string
    SupportedIdentityTypes() []string
    BuildClientAuth(identity IdentityConfig, jwtSrc JWTSource) (ClientAuth, error)
}
```

| Method | Purpose |
|--------|---------|
| `Name()` | Provider identifier used in config (e.g. `"keycloak"`) |
| `TokenEndpoint()` | Derives the OAuth token endpoint URL from provider base URL and realm/tenant. Returns `""` if inputs are insufficient. |
| `DefaultAssertionType()` | Default `client_assertion_type` short name for SPIFFE identity (e.g. `"jwt-spiffe"` for Keycloak, `"jwt-bearer"` for Okta). Returns `""` if JWT assertions are not supported. |
| `SupportedIdentityTypes()` | Identity types this provider supports (e.g. `["client-secret", "spiffe"]`). Used at `Configure()` to reject unsupported combinations early. |
| `BuildClientAuth()` | Constructs the provider-appropriate `exchange.ClientAuth` from the identity config. Each provider owns its auth strategy. |

## Adding a New IdP Provider

Create a single file — no changes to core plugin code required:

```go
// provider_okta.go
package tokenexchange

import (
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/tokenexchange/exchange"
    fwspiffe "github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
)

type oktaProvider struct{}

func (oktaProvider) Name() string { return "okta" }

func (oktaProvider) TokenEndpoint(providerURL, providerRealm string) string {
    // Okta URL derivation logic
}

func (oktaProvider) DefaultAssertionType() string { return JWTBearerAssertion }

func (oktaProvider) SupportedIdentityTypes() []string {
    return []string{ClientSecretIdentity, SpiffeIdentity}
}

func (oktaProvider) BuildClientAuth(id IdentityConfig, jwtSrc fwspiffe.JWTSource) (exchange.ClientAuth, error) {
    // Okta-specific auth construction
}

func init() { RegisterProvider(oktaProvider{}) }
```

The `init()` auto-registration pattern means any provider file compiled
into the binary is automatically available — no central list to maintain.
A CI test (`TestAllProviderFilesAreRegistered`) scans `provider_*.go`
files and verifies each has `RegisterProvider()` in `init()`.

## Configuration

```yaml
token-exchange:
  # Explicit URL (works for any IdP, always wins)
  token_url: "https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token"

  # OR: provider-assisted derivation (convenience for registered IdPs)
  provider: "keycloak"       # must be a registered provider name
  provider_url: "https://keycloak.example.com"
  provider_realm: "my-realm"

  identity:
    type: "client-secret"    # must be in provider's SupportedIdentityTypes()
    client_id: "my-agent"
    client_secret: "..."
    # assertion_type: "jwt-bearer"  # optional, defaults to provider's DefaultAssertionType()
```

### Resolution chain

1. **Explicit `token_url`** — always wins (works for any IdP)
2. **Provider derivation** — `LookupProvider(provider).TokenEndpoint(url, realm)`
3. **Per-route `token_url` override** — in `routes.yaml`, per-host

### Backward compatibility

`keycloak_url` and `keycloak_realm` continue to work. When present
and `provider` is not set, the plugin infers `provider: "keycloak"`.
A deprecation warning is logged.

### Validation at Configure() time

- Unknown provider names → rejected (`"provider X is not registered"`)
- Identity type not in `provider.SupportedIdentityTypes()` → rejected
- Unknown `assertion_type` → rejected (must be in `AssertionTypeURN` map)
- Missing `token_url` after derivation → rejected

## Available Constants

```go
// Identity types
ClientSecretIdentity = "client-secret"
SpiffeIdentity       = "spiffe"

// Assertion types (keys into AssertionTypeURN)
JWTSpiffeAssertion = "jwt-spiffe"
JWTBearerAssertion = "jwt-bearer"

// Policies
PassthroughPolicy = "passthrough"
ExchangePolicy    = "exchange"

// Providers
KeycloakProvider = "keycloak"
GenericProvider  = "generic"

// Defaults
DefaultAssertion = JWTSpiffeAssertion
DefaultProvider  = KeycloakProvider
```

## Reference Implementation: Keycloak

See `provider_keycloak.go` for the reference implementation:

- `TokenEndpoint`: `{url}/realms/{realm}/protocol/openid-connect/token`
- `DefaultAssertionType`: `jwt-spiffe`
- `SupportedIdentityTypes`: `[client-secret, spiffe]`
- `BuildClientAuth`: `ClientSecretAuth` or `JWTAssertionAuth` with `jwt-spiffe` URN

## What does NOT change

- **RFC 8693 token exchange parameters** — standard across all IdPs
- **Route resolver** — host-to-audience matching, per-route overrides
- **Token cache** — SHA-256 keyed, IdP-agnostic
- **Plugin registry** — `plugins.RegisterPlugin("token-exchange", ...)`
- **SPIFFE provider injection** — `SetSPIFFEProvider` / `plugins.BuildWithSPIFFE`
- **Credential file handling** — `/shared/client-id.txt`, `/shared/client-secret.txt`
- **Error handling** — standard OAuth error response parsing (RFC 6749)

## Per-IdP Auth Method Matrix

| IdP | `client-secret` | `spiffe` (JWT assertion) | `certificate` | Default assertion |
|-----|----------------|------------------------|---------------|-------------------|
| **Keycloak** | ✅ | ✅ (`jwt-spiffe`) | ❌ | `jwt-spiffe` |
| **Okta** (future) | ✅ | ✅ (`jwt-bearer`) | ❌ | `jwt-bearer` |
| **Entra ID** (future) | ✅ | ❌ | ✅ (future) | N/A |

## Testing strategy

Each IdP provider should include:
1. **Unit tests** — `TokenEndpoint()` derivation for various inputs
2. **`BuildClientAuth` tests** — verify correct `ClientAuth` construction
3. **Validation tests** — unsupported identity types rejected
4. **Registration guard** — `TestAllProviderFilesAreRegistered` catches missing `init()`
5. **Integration tests** (optional) — against a real or emulated IdP

See `plugin_test.go` for the Keycloak provider test patterns.
