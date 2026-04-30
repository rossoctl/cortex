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
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name: "valid JWT config",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "jwt",
				Role:       "test-role",
			},
			wantErr: false,
		},
		{
			name: "valid Kubernetes config",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "kubernetes",
				Role:       "test-role",
			},
			wantErr: false,
		},
		{
			name: "valid token config",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "token",
				Token:      "hvs.test",
			},
			wantErr: false,
		},
		{
			name: "missing address",
			config: &Config{
				AuthMethod: "jwt",
				Role:       "test-role",
			},
			wantErr: true,
		},
		{
			name: "missing auth method",
			config: &Config{
				Address: "https://vault.example.com",
			},
			wantErr: true,
		},
		{
			name: "JWT auth missing role",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "jwt",
			},
			wantErr: true,
		},
		{
			name: "token auth missing token",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "token",
			},
			wantErr: true,
		},
		{
			name: "unsupported auth method",
			config: &Config{
				Address:    "https://vault.example.com",
				AuthMethod: "invalid",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := &Config{
		Address:    "https://vault.example.com",
		AuthMethod: "jwt",
		Role:       "test-role",
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() failed: %v", err)
	}

	// Check defaults were applied
	if cfg.JWTPath != "/opt/jwt_svid.token" {
		t.Errorf("JWTPath default not applied, got %s", cfg.JWTPath)
	}

	if cfg.JWTAudience != "vault" {
		t.Errorf("JWTAudience default not applied, got %s", cfg.JWTAudience)
	}

	if cfg.CacheTTL != 5*time.Minute {
		t.Errorf("CacheTTL default not applied, got %s", cfg.CacheTTL)
	}
}
