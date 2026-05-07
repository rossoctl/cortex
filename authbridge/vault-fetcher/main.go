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

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/vault"
	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

// Config represents the vault-fetcher configuration
type Config struct {
	Vault         vault.Config  `yaml:"vault"`
	Secrets       []SecretSpec  `yaml:"secrets"`
	OutputFormats OutputFormats `yaml:"output_formats"`
}

// SecretSpec specifies a secret to fetch and where to write it
type SecretSpec struct {
	Path   string `yaml:"path"`   // Vault secret path (e.g., "secret/data/github/token")
	Field  string `yaml:"field"`  // Field name in the secret
	Output string `yaml:"output"` // Output file path (e.g., "/shared/secrets/github-token")
	Mode   string `yaml:"mode"`   // File permissions (e.g., "0600")
}

// OutputFormats specifies optional output formats
type OutputFormats struct {
	EnvFile  *EnvFileConfig  `yaml:"env_file"`
	JSONFile *JSONFileConfig `yaml:"json_file"`
}

// EnvFileConfig configures environment file output
type EnvFileConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// JSONFileConfig configures JSON file output
type JSONFileConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

func main() {
	// Parse flags
	configFile := flag.String("config", "/etc/vault-fetcher/config.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	// Handle version flag
	if *showVersion {
		fmt.Printf("vault-fetcher version %s\n", version)
		os.Exit(0)
	}

	// Set up logging
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[vault-fetcher] ")

	log.Printf("Starting vault-fetcher v%s", version)

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Loaded config from %s", *configFile)

	// Apply environment variable overrides
	applyEnvOverrides(&cfg.Vault)

	// Validate configuration
	if err := cfg.Vault.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	if len(cfg.Secrets) == 0 {
		log.Fatalf("No secrets configured to fetch")
	}

	log.Printf("Configured to fetch %d secret(s)", len(cfg.Secrets))

	// Create Vault client
	ctx := context.Background()
	client, err := vault.NewClient(&cfg.Vault)
	if err != nil {
		log.Fatalf("Failed to create Vault client: %v", err)
	}
	defer client.Close()

	log.Printf("Created Vault client (address=%s, auth_method=%s)", cfg.Vault.Address, cfg.Vault.AuthMethod)

	// Authenticate to Vault
	if err := authenticateWithRetry(ctx, client, 3); err != nil {
		log.Fatalf("Failed to authenticate to Vault: %v", err)
	}

	log.Printf("Successfully authenticated to Vault")

	// Fetch secrets
	secretsMap := make(map[string]string)
	for i, spec := range cfg.Secrets {
		log.Printf("[%d/%d] Fetching secret: %s (field: %s)", i+1, len(cfg.Secrets), spec.Path, spec.Field)

		value, leaseDuration, err := client.ReadSecret(ctx, spec.Path, spec.Field)
		if err != nil {
			log.Fatalf("Failed to fetch secret %s: %v", spec.Path, err)
		}

		log.Printf("[%d/%d] Secret fetched (lease: %ds)", i+1, len(cfg.Secrets), leaseDuration)

		// Write to individual file
		if err := writeSecretToFile(spec.Output, value, spec.Mode); err != nil {
			log.Fatalf("Failed to write secret to %s: %v", spec.Output, err)
		}

		log.Printf("[%d/%d] Written to: %s", i+1, len(cfg.Secrets), spec.Output)

		// Store in map for optional output formats
		// Use the field name as the key (e.g., "token" -> "TOKEN" in env file)
		secretsMap[spec.Field] = value
	}

	// Write optional output formats
	if cfg.OutputFormats.EnvFile != nil && cfg.OutputFormats.EnvFile.Enabled {
		if err := writeEnvFile(cfg.OutputFormats.EnvFile.Path, secretsMap); err != nil {
			log.Fatalf("Failed to write env file: %v", err)
		}
		log.Printf("Written env file: %s", cfg.OutputFormats.EnvFile.Path)
	}

	if cfg.OutputFormats.JSONFile != nil && cfg.OutputFormats.JSONFile.Enabled {
		if err := writeJSONFile(cfg.OutputFormats.JSONFile.Path, secretsMap); err != nil {
			log.Fatalf("Failed to write JSON file: %v", err)
		}
		log.Printf("Written JSON file: %s", cfg.OutputFormats.JSONFile.Path)
	}

	log.Printf("All secrets fetched successfully")
}

// loadConfig loads configuration from a YAML file
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}

// applyEnvOverrides applies environment variable overrides to vault config
func applyEnvOverrides(cfg *vault.Config) {
	if addr := os.Getenv("VAULT_ADDR"); addr != "" {
		cfg.Address = addr
		log.Printf("Using VAULT_ADDR from environment: %s", addr)
	}

	if role := os.Getenv("VAULT_ROLE"); role != "" {
		cfg.Role = role
		log.Printf("Using VAULT_ROLE from environment: %s", role)
	}

	if jwtPath := os.Getenv("JWT_PATH"); jwtPath != "" {
		cfg.JWTPath = jwtPath
		log.Printf("Using JWT_PATH from environment: %s", jwtPath)
	}

	if token := os.Getenv("VAULT_TOKEN"); token != "" {
		cfg.Token = token
		log.Printf("Using VAULT_TOKEN from environment")
	}
}

// authenticateWithRetry attempts to authenticate with exponential backoff
func authenticateWithRetry(ctx context.Context, client *vault.Client, maxRetries int) error {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := client.Authenticate(ctx)
		if err == nil {
			return nil
		}

		lastErr = err
		if attempt < maxRetries {
			backoff := time.Duration(attempt*2) * time.Second
			log.Printf("Authentication failed (attempt %d/%d): %v. Retrying in %s...",
				attempt, maxRetries, err, backoff)
			time.Sleep(backoff)
		}
	}

	return fmt.Errorf("authentication failed after %d attempts: %w", maxRetries, lastErr)
}

// writeSecretToFile writes a secret value to a file with specified permissions
func writeSecretToFile(path, value, mode string) error {
	// Create parent directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Parse file mode (default to 0600 for security)
	fileMode := os.FileMode(0600)
	if mode != "" {
		// Parse octal mode string (e.g., "0600")
		modeInt, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid file mode %s: %w", mode, err)
		}
		fileMode = os.FileMode(modeInt)
	}

	// Write file
	if err := os.WriteFile(path, []byte(value), fileMode); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// writeEnvFile writes secrets to a shell-compatible environment file
func writeEnvFile(path string, secrets map[string]string) error {
	// Create parent directory
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Open file
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()

	// Write header
	fmt.Fprintf(f, "# Generated by vault-fetcher at %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "# Do not edit manually\n\n")

	// Write secrets
	for key, value := range secrets {
		// Convert field name to uppercase env var name
		envKey := toEnvVarName(key)
		fmt.Fprintf(f, "%s=%s\n", envKey, value)
	}

	return nil
}

// writeJSONFile writes secrets to a JSON file
func writeJSONFile(path string, secrets map[string]string) error {
	// Create parent directory
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Marshal to JSON
	data, err := yaml.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Write file
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// toEnvVarName converts a field name to an environment variable name
// Examples: "token" -> "TOKEN", "api-key" -> "API_KEY"
func toEnvVarName(field string) string {
	// Simple implementation: uppercase and replace hyphens with underscores
	result := ""
	for _, ch := range field {
		if ch == '-' || ch == '.' {
			result += "_"
		} else if ch >= 'a' && ch <= 'z' {
			result += string(ch - 32) // Convert to uppercase
		} else {
			result += string(ch)
		}
	}
	return result
}
