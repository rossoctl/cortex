// Copyright 2026 Kagenti Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vault

import (
	"fmt"
	"time"
)

// Config contains the configuration for the Vault client
type Config struct {
	// Address is the Vault server address (e.g., "https://vault.example.com")
	Address string `yaml:"address"`

	// AuthMethod specifies the authentication method: "jwt", "kubernetes", or "token"
	AuthMethod string `yaml:"auth_method"`

	// Role is the Vault role name for JWT or Kubernetes authentication
	Role string `yaml:"role"`

	// JWTPath is the path to the JWT-SVID file (for JWT auth method)
	// Default: /opt/jwt_svid.token
	JWTPath string `yaml:"jwt_path"`

	// JWTAudience is the expected audience claim in the JWT (for JWT auth method)
	// Default: vault
	JWTAudience string `yaml:"jwt_audience"`

	// KubernetesTokenPath is the path to the Kubernetes service account token (for Kubernetes auth method)
	// Default: /var/run/secrets/kubernetes.io/serviceaccount/token
	KubernetesTokenPath string `yaml:"kubernetes_token_path"`

	// Token is the Vault token for direct token authentication
	Token string `yaml:"token"`

	// CacheTTL is the maximum duration to cache secrets
	// Actual cache duration is min(CacheTTL, Vault lease duration)
	CacheTTL time.Duration `yaml:"cache_ttl"`

	// Namespace is the Vault namespace (Vault Enterprise feature)
	Namespace string `yaml:"namespace"`

	// TLSSkipVerify disables TLS certificate verification (for development only)
	TLSSkipVerify bool `yaml:"tls_skip_verify"`

	// CACert is the path to a CA certificate file for TLS verification
	CACert string `yaml:"ca_cert"`

	// ClientCert is the path to a client certificate file for mTLS
	ClientCert string `yaml:"client_cert"`

	// ClientKey is the path to a client key file for mTLS
	ClientKey string `yaml:"client_key"`
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Address == "" {
		return fmt.Errorf("vault address is required")
	}

	if c.AuthMethod == "" {
		return fmt.Errorf("auth method is required")
	}

	switch c.AuthMethod {
	case "jwt":
		if c.Role == "" {
			return fmt.Errorf("role is required for JWT auth method")
		}
		if c.JWTPath == "" {
			c.JWTPath = "/opt/jwt_svid.token"
		}
		if c.JWTAudience == "" {
			c.JWTAudience = "vault"
		}
	case "kubernetes":
		if c.Role == "" {
			return fmt.Errorf("role is required for Kubernetes auth method")
		}
		if c.KubernetesTokenPath == "" {
			c.KubernetesTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		}
	case "token":
		if c.Token == "" {
			return fmt.Errorf("token is required for token auth method")
		}
	default:
		return fmt.Errorf("unsupported auth method: %s (supported: jwt, kubernetes, token)", c.AuthMethod)
	}

	if c.CacheTTL == 0 {
		c.CacheTTL = 5 * time.Minute
	}

	return nil
}

// SecretRequest represents a request to read a secret from Vault
type SecretRequest struct {
	// Path is the full path to the secret (e.g., "secret/data/github/token")
	Path string `yaml:"path"`

	// Field is the field name to extract from the secret
	Field string `yaml:"field"`
}

// SecretResponse represents a secret value retrieved from Vault
type SecretResponse struct {
	// Value is the secret value
	Value string

	// LeaseDuration is the lease duration in seconds (0 if not renewable)
	LeaseDuration int64

	// Path is the secret path (from the request)
	Path string

	// Field is the field name (from the request)
	Field string
}
