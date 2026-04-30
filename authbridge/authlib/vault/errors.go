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

import "fmt"

// Error types for better error handling

// AuthError represents an authentication failure
type AuthError struct {
	Method string
	Reason string
	Err    error
}

func (e *AuthError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("vault auth failed (method=%s): %s: %v", e.Method, e.Reason, e.Err)
	}
	return fmt.Sprintf("vault auth failed (method=%s): %s", e.Method, e.Reason)
}

func (e *AuthError) Unwrap() error {
	return e.Err
}

// SecretNotFoundError represents a secret that doesn't exist
type SecretNotFoundError struct {
	Path string
}

func (e *SecretNotFoundError) Error() string {
	return fmt.Sprintf("secret not found at path: %s", e.Path)
}

// FieldNotFoundError represents a field that doesn't exist in a secret
type FieldNotFoundError struct {
	Path  string
	Field string
}

func (e *FieldNotFoundError) Error() string {
	return fmt.Sprintf("field %s not found in secret at path: %s", e.Field, e.Path)
}

// ConfigError represents a configuration error
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config error: %s: %s", e.Field, e.Reason)
}
