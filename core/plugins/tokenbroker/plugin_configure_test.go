package tokenbroker

import (
	"encoding/json"
	"strings"
	"testing"
)

// =============================================================================
// Configuration Tests
// =============================================================================

func TestTokenBroker_Name(t *testing.T) {
	p := NewTokenBroker()
	if p.Name() != "token-broker" {
		t.Errorf("Name() = %q, want %q", p.Name(), "token-broker")
	}
}

func TestTokenBroker_Capabilities(t *testing.T) {
	p := NewTokenBroker()
	caps := p.Capabilities()
	t.Logf("TokenBroker capabilities: %+v", caps)
}

func TestTokenBroker_Configure_Valid(t *testing.T) {
	routesFile, err := writeTempRoutesFile(t, `
- host: file.example.com
  action: passthrough
`)
	if err != nil {
		t.Fatalf("writeTempRoutesFile() error = %v", err)
	}

	tests := []struct {
		name   string
		config string
	}{
		{
			name: "minimal config",
			config: `{
				"broker_url": "http://broker:8080"
			}`,
		},
		{
			name: "with default policy",
			config: `{
				"broker_url": "http://broker:8080",
				"default_policy": "broker"
			}`,
		},
		{
			name: "with inline routes",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"rules": [
						{"host": "api.example.com", "action": "broker"}
					]
				}
			}`,
		},
		{
			name: "with routes file",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"file": "` + routesFile + `"
				}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewTokenBroker()
			err := p.Configure(json.RawMessage(tt.config))
			if err != nil {
				t.Errorf("Configure() error = %v, want nil", err)
			}
		})
	}
}

func TestTokenBroker_Configure_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "missing broker_url",
			config:  `{}`,
			wantErr: "broker_url is required",
		},
		{
			name: "invalid default_policy",
			config: `{
				"broker_url": "http://broker:8080",
				"default_policy": "invalid"
			}`,
			wantErr: "default_policy must be broker or passthrough",
		},
		{
			name:    "invalid json",
			config:  `{invalid}`,
			wantErr: "token-broker config",
		},
		{
			name: "unknown field",
			config: `{
				"broker_url": "http://broker:8080",
				"unknown_field": "value"
			}`,
			wantErr: "token-broker config",
		},
		{
			name: "invalid route pattern",
			config: `{
				"broker_url": "http://broker:8080",
				"routes": {
					"rules": [
						{"host": "[", "action": "broker"}
					]
				}
			}`,
			wantErr: "token-broker routes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewTokenBroker()
			err := p.Configure(json.RawMessage(tt.config))
			if err == nil {
				t.Error("Configure() error = nil, want error")
				return
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Configure() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestTokenBroker_Configure_NonExistentRoutesFile(t *testing.T) {
	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"routes": {
			"file": "/nonexistent/routes.yaml"
		}
	}`

	err := p.Configure(json.RawMessage(config))
	if err == nil {
		t.Error("Configure() with non-existent routes file should return error")
		return
	}
	if !strings.Contains(err.Error(), "routes") {
		t.Errorf("Configure() error should mention routes, got: %v", err)
	}
}

func TestTokenBroker_Configure_InvalidRouteFile(t *testing.T) {
	routesFile, err := writeTempRoutesFile(t, `
invalid yaml content [
  - this is not valid
`)
	if err != nil {
		t.Fatalf("writeTempRoutesFile() error = %v", err)
	}

	p := NewTokenBroker()
	config := `{
		"broker_url": "http://broker:8080",
		"routes": {
			"file": "` + routesFile + `"
		}
	}`

	err = p.Configure(json.RawMessage(config))
	if err == nil {
		t.Error("Configure() with invalid routes file should return error")
	}
}
