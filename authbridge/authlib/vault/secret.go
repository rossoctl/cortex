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
	"fmt"

	vault "github.com/hashicorp/vault/api"
)

// SecretReader handles reading secrets from Vault with KV v1/v2 auto-detection
type SecretReader struct {
	client *vault.Client
}

// NewSecretReader creates a new secret reader
func NewSecretReader(client *vault.Client) *SecretReader {
	return &SecretReader{
		client: client,
	}
}

// ReadSecret reads a secret from Vault and extracts the specified field
//
// This function auto-detects KV v1 vs KV v2 secret engines:
// - KV v1: secret.Data[field]
// - KV v2: secret.Data["data"][field]
//
// Pattern adapted from Klaviger (github.com/grs/klaviger/internal/tokeninjector/vault_injector.go)
func (r *SecretReader) ReadSecret(ctx context.Context, path, field string) (string, int64, error) {
	// Read secret from Vault
	secret, err := r.client.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read secret: %w", err)
	}

	if secret == nil {
		return "", 0, &SecretNotFoundError{Path: path}
	}

	// Extract value from secret data
	value, err := r.extractField(secret, path, field)
	if err != nil {
		return "", 0, err
	}

	// Return value and lease duration
	leaseDuration := int64(secret.LeaseDuration)
	return value, leaseDuration, nil
}

// extractField extracts a field from a Vault secret, handling both KV v1 and KV v2
func (r *SecretReader) extractField(secret *vault.Secret, path, field string) (string, error) {
	var value string

	// Check if this is a KV v2 secret (has "data" wrapper)
	// KV v2 structure: secret.Data["data"][field]
	if data, ok := secret.Data["data"].(map[string]interface{}); ok {
		// KV v2
		if val, ok := data[field]; ok {
			value, ok = val.(string)
			if !ok {
				return "", fmt.Errorf("field %s is not a string (type=%T)", field, val)
			}
		}
	} else {
		// KV v1 or other secret engine
		// KV v1 structure: secret.Data[field]
		if val, ok := secret.Data[field]; ok {
			value, ok = val.(string)
			if !ok {
				return "", fmt.Errorf("field %s is not a string (type=%T)", field, val)
			}
		}
	}

	if value == "" {
		return "", &FieldNotFoundError{Path: path, Field: field}
	}

	return value, nil
}

// ReadSecrets reads multiple secrets in batch
func (r *SecretReader) ReadSecrets(ctx context.Context, requests []SecretRequest) ([]SecretResponse, error) {
	responses := make([]SecretResponse, 0, len(requests))

	for _, req := range requests {
		value, leaseDuration, err := r.ReadSecret(ctx, req.Path, req.Field)
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

// ListSecrets lists secret paths at the given path (useful for discovery)
func (r *SecretReader) ListSecrets(ctx context.Context, path string) ([]string, error) {
	secret, err := r.client.Logical().ListWithContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets: %w", err)
	}

	if secret == nil || secret.Data == nil {
		return []string{}, nil
	}

	keys, ok := secret.Data["keys"].([]interface{})
	if !ok {
		return []string{}, nil
	}

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if keyStr, ok := key.(string); ok {
			result = append(result, keyStr)
		}
	}

	return result, nil
}
