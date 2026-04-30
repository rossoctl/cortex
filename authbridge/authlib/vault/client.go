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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"

	vault "github.com/hashicorp/vault/api"
)

// Client is the main Vault client that combines authentication, secret reading, and caching
type Client struct {
	vaultClient *vault.Client
	config      *Config
	logger      *log.Logger

	authenticator *Authenticator
	secretReader  *SecretReader
	cache         *SecretCache
}

// NewClient creates a new Vault client
func NewClient(cfg *Config) (*Client, error) {
	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, &ConfigError{
			Field:  "config",
			Reason: err.Error(),
		}
	}

	// Create logger
	logger := log.New(os.Stderr, "[Vault] ", log.LstdFlags)

	// Create Vault client configuration
	vaultConfig := vault.DefaultConfig()
	vaultConfig.Address = cfg.Address

	// Configure TLS if needed
	if cfg.TLSSkipVerify || cfg.CACert != "" || cfg.ClientCert != "" {
		tlsConfig, err := configureTLS(cfg)
		if err != nil {
			return nil, &ConfigError{
				Field:  "tls",
				Reason: fmt.Sprintf("failed to configure TLS: %v", err),
			}
		}

		vaultConfig.HttpClient.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
	}

	// Create Vault client
	vaultClient, err := vault.NewClient(vaultConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create vault client: %w", err)
	}

	// Set namespace if configured (Vault Enterprise feature)
	if cfg.Namespace != "" {
		vaultClient.SetNamespace(cfg.Namespace)
	}

	// Create components
	authenticator := NewAuthenticator(vaultClient, cfg, logger)
	secretReader := NewSecretReader(vaultClient)
	cache := NewSecretCache(cfg.CacheTTL)

	return &Client{
		vaultClient:   vaultClient,
		config:        cfg,
		logger:        logger,
		authenticator: authenticator,
		secretReader:  secretReader,
		cache:         cache,
	}, nil
}

// Authenticate performs authentication based on the configured method
func (c *Client) Authenticate(ctx context.Context) error {
	return c.authenticator.Authenticate(ctx)
}

// ReadSecret reads a secret from Vault with caching
//
// The cache uses lease-aware TTL: min(configured TTL, Vault lease duration)
func (c *Client) ReadSecret(ctx context.Context, path, field string) (string, int64, error) {
	// Check cache first
	if value, ok := c.cache.Get(path); ok {
		c.logger.Printf("cache hit for secret: %s", path)
		return value, 0, nil // Return 0 for lease duration on cache hit
	}

	c.logger.Printf("cache miss for secret: %s", path)

	// Read from Vault
	value, leaseDuration, err := c.secretReader.ReadSecret(ctx, path, field)
	if err != nil {
		return "", 0, err
	}

	// Store in cache
	c.cache.Set(path, value, leaseDuration)

	return value, leaseDuration, nil
}

// ReadSecrets reads multiple secrets in batch
func (c *Client) ReadSecrets(ctx context.Context, requests []SecretRequest) ([]SecretResponse, error) {
	responses := make([]SecretResponse, 0, len(requests))

	for _, req := range requests {
		value, leaseDuration, err := c.ReadSecret(ctx, req.Path, req.Field)
		if err != nil {
			return nil, fmt.Errorf("failed to read secret %s: %w", req.Path, err)
		}

		responses = append(responses, SecretResponse{
			Value:         value,
			LeaseDuration: leaseDuration,
			Path:          req.Path,
			Field:         req.Field,
		})
	}

	return responses, nil
}

// ListSecrets lists secret paths at the given path
func (c *Client) ListSecrets(ctx context.Context, path string) ([]string, error) {
	return c.secretReader.ListSecrets(ctx, path)
}

// Close stops token renewal and clears the cache
func (c *Client) Close() error {
	c.authenticator.Stop()
	c.cache.Clear()
	return nil
}

// GetVaultClient returns the underlying Vault client (for advanced usage)
func (c *Client) GetVaultClient() *vault.Client {
	return c.vaultClient
}

// ClearCache clears the secret cache
func (c *Client) ClearCache() {
	c.cache.Clear()
}

// configureTLS creates a TLS configuration based on the config
func configureTLS(cfg *Config) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.TLSSkipVerify,
	}

	// Load CA certificate if provided
	if cfg.CACert != "" {
		caCert, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}

		tlsConfig.RootCAs = caCertPool
	}

	// Load client certificate if provided (mTLS)
	if cfg.ClientCert != "" && cfg.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert/key: %w", err)
		}

		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}
