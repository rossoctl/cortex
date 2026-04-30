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

// Package vault provides Vault integration for Kagenti workloads.
//
// This implementation follows patterns from:
// - Hashicorp Vault Go SDK documentation
// - Klaviger project (github.com/grs/klaviger) - studied as prior art
// - IBM Trusted Service Identity examples (github.com/IBM/trusted-service-identity)
//
// Key patterns adapted from Klaviger:
// - KV v1/v2 auto-detection
// - Lease-aware caching
// - Kubernetes SA authentication
//
// Key additions for Kagenti:
// - JWT/OIDC authentication (SPIFFE pattern from IBM TSI)
// - Token renewal with goroutine
// - Integration with authlib/cache
package vault

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	vault "github.com/hashicorp/vault/api"
)

// Authenticator handles Vault authentication with token renewal
type Authenticator struct {
	client *vault.Client
	config *Config
	logger *log.Logger

	// Token renewal
	mu             sync.RWMutex
	currentToken   string
	tokenRenewable bool
	tokenTTL       time.Duration
	stopRenewal    chan struct{}
	renewalWG      sync.WaitGroup
}

// NewAuthenticator creates a new authenticator
func NewAuthenticator(client *vault.Client, config *Config, logger *log.Logger) *Authenticator {
	if logger == nil {
		logger = log.New(os.Stderr, "[Vault Auth] ", log.LstdFlags)
	}

	return &Authenticator{
		client:      client,
		config:      config,
		logger:      logger,
		stopRenewal: make(chan struct{}),
	}
}

// Authenticate performs authentication based on the configured method
func (a *Authenticator) Authenticate(ctx context.Context) error {
	switch a.config.AuthMethod {
	case "jwt":
		return a.authenticateJWT(ctx)
	case "kubernetes":
		return a.authenticateKubernetes(ctx)
	case "token":
		return a.authenticateToken(ctx)
	default:
		return &AuthError{
			Method: a.config.AuthMethod,
			Reason: "unsupported auth method",
		}
	}
}

// authenticateJWT performs JWT/OIDC authentication using SPIFFE JWT-SVID
//
// This implements the SPIFFE-to-Vault authentication pattern from IBM TSI:
// 1. Read JWT-SVID from file (written by spiffe-helper)
// 2. Call Vault's JWT auth backend at /auth/jwt/login
// 3. Receive Vault client token
// 4. Start token renewal if the token is renewable
func (a *Authenticator) authenticateJWT(ctx context.Context) error {
	a.logger.Printf("authenticating with JWT method (role=%s, jwt_path=%s)", a.config.Role, a.config.JWTPath)

	// Read JWT-SVID from file
	jwtBytes, err := os.ReadFile(a.config.JWTPath)
	if err != nil {
		return &AuthError{
			Method: "jwt",
			Reason: "failed to read JWT-SVID file",
			Err:    err,
		}
	}

	jwt := string(jwtBytes)
	if jwt == "" {
		return &AuthError{
			Method: "jwt",
			Reason: "JWT-SVID file is empty",
		}
	}

	// Prepare auth request
	data := map[string]interface{}{
		"role": a.config.Role,
		"jwt":  jwt,
	}

	// Authenticate to Vault's JWT auth backend
	secret, err := a.client.Logical().WriteWithContext(ctx, "auth/jwt/login", data)
	if err != nil {
		return &AuthError{
			Method: "jwt",
			Reason: "JWT auth request failed",
			Err:    err,
		}
	}

	if secret == nil || secret.Auth == nil {
		return &AuthError{
			Method: "jwt",
			Reason: "no auth info returned from vault",
		}
	}

	// Set token
	a.client.SetToken(secret.Auth.ClientToken)

	a.mu.Lock()
	a.currentToken = secret.Auth.ClientToken
	a.tokenRenewable = secret.Auth.Renewable
	a.tokenTTL = time.Duration(secret.Auth.LeaseDuration) * time.Second
	a.mu.Unlock()

	a.logger.Printf("JWT auth successful (lease_duration=%s, renewable=%v)",
		a.tokenTTL, a.tokenRenewable)

	// Start token renewal if renewable
	if secret.Auth.Renewable {
		a.startTokenRenewal(secret.Auth.LeaseDuration)
	}

	return nil
}

// authenticateKubernetes performs Kubernetes service account authentication
//
// Pattern adapted from Klaviger: reads the Kubernetes SA token and uses
// Vault's Kubernetes auth backend.
func (a *Authenticator) authenticateKubernetes(ctx context.Context) error {
	a.logger.Printf("authenticating with Kubernetes method (role=%s)", a.config.Role)

	// Read service account token
	jwtBytes, err := os.ReadFile(a.config.KubernetesTokenPath)
	if err != nil {
		return &AuthError{
			Method: "kubernetes",
			Reason: "failed to read service account token",
			Err:    err,
		}
	}

	jwt := string(jwtBytes)
	if jwt == "" {
		return &AuthError{
			Method: "kubernetes",
			Reason: "service account token file is empty",
		}
	}

	// Prepare auth request
	data := map[string]interface{}{
		"role": a.config.Role,
		"jwt":  jwt,
	}

	// Authenticate to Vault's Kubernetes auth backend
	secret, err := a.client.Logical().WriteWithContext(ctx, "auth/kubernetes/login", data)
	if err != nil {
		return &AuthError{
			Method: "kubernetes",
			Reason: "Kubernetes auth request failed",
			Err:    err,
		}
	}

	if secret == nil || secret.Auth == nil {
		return &AuthError{
			Method: "kubernetes",
			Reason: "no auth info returned from vault",
		}
	}

	// Set token
	a.client.SetToken(secret.Auth.ClientToken)

	a.mu.Lock()
	a.currentToken = secret.Auth.ClientToken
	a.tokenRenewable = secret.Auth.Renewable
	a.tokenTTL = time.Duration(secret.Auth.LeaseDuration) * time.Second
	a.mu.Unlock()

	a.logger.Printf("Kubernetes auth successful (lease_duration=%s, renewable=%v)",
		a.tokenTTL, a.tokenRenewable)

	// Start token renewal if renewable
	if secret.Auth.Renewable {
		a.startTokenRenewal(secret.Auth.LeaseDuration)
	}

	return nil
}

// authenticateToken uses direct token authentication
func (a *Authenticator) authenticateToken(ctx context.Context) error {
	a.logger.Printf("authenticating with token method")

	a.client.SetToken(a.config.Token)

	a.mu.Lock()
	a.currentToken = a.config.Token
	// For direct token auth, we don't know if it's renewable without checking
	// For now, assume it's not renewable
	a.tokenRenewable = false
	a.tokenTTL = 0
	a.mu.Unlock()

	a.logger.Printf("token auth successful")

	return nil
}

// startTokenRenewal starts a background goroutine to renew the Vault token
//
// The token is renewed at 2/3 of its lease duration to ensure it doesn't expire.
// This implements the token renewal pattern that was a TODO in Klaviger.
func (a *Authenticator) startTokenRenewal(leaseDuration int) {
	// Renew at 2/3 of lease duration (Vault best practice)
	renewInterval := time.Duration(leaseDuration) * time.Second * 2 / 3

	a.logger.Printf("starting token renewal (interval=%s)", renewInterval)

	a.renewalWG.Add(1)
	go func() {
		defer a.renewalWG.Done()

		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := a.renewToken(); err != nil {
					a.logger.Printf("failed to renew token: %v", err)
					// Note: We log the error but continue trying to renew
					// In production, you might want to implement exponential backoff
					// or trigger re-authentication after N failures
				}
			case <-a.stopRenewal:
				a.logger.Printf("stopping token renewal")
				return
			}
		}
	}()
}

// renewToken renews the current Vault token
func (a *Authenticator) renewToken() error {
	a.mu.RLock()
	leaseDuration := int(a.tokenTTL.Seconds())
	a.mu.RUnlock()

	secret, err := a.client.Auth().Token().RenewSelf(leaseDuration)
	if err != nil {
		return fmt.Errorf("token renewal failed: %w", err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("no auth info returned from token renewal")
	}

	// Update token info
	a.mu.Lock()
	a.currentToken = secret.Auth.ClientToken
	a.tokenTTL = time.Duration(secret.Auth.LeaseDuration) * time.Second
	a.mu.Unlock()

	a.logger.Printf("token renewed successfully (new_lease=%s)", a.tokenTTL)

	return nil
}

// Stop stops the token renewal goroutine
func (a *Authenticator) Stop() {
	close(a.stopRenewal)
	a.renewalWG.Wait()
}

// GetCurrentToken returns the current Vault token (thread-safe)
func (a *Authenticator) GetCurrentToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.currentToken
}
