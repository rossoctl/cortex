package main

import (
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// gatewayTokenEnv names the file that holds the bearer token cortex-gateway sends on
// POST /v1/ingest. The supervisor creates the file; Cortex only reads it.
const gatewayTokenEnv = "CORTEX_GATEWAY_TOKEN_FILE"

// readGatewayToken reads the token file that CORTEX_GATEWAY_TOKEN_FILE names.
func readGatewayToken(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	path := getenv(gatewayTokenEnv)
	if path == "" {
		return "", fmt.Errorf("praxis.enabled needs %s to name the gateway token file", gatewayTokenEnv)
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("read the gateway token file (%s): %w", gatewayTokenEnv, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("the gateway token file %s (%s) is empty", path, gatewayTokenEnv)
	}
	return token, nil
}

// ingestOptions turns on the ingestion endpoint in praxis mode, with the hop's joins
// (nil when no forward proxy runs). In direct mode it returns nil.
func ingestOptions(cfg *config.Config, rates pricing.Resolver, token string, joins sessionapi.HopJoins) []sessionapi.Option {
	if !cfg.PraxisEnabled() {
		return nil
	}
	return []sessionapi.Option{sessionapi.WithIngest(sessionapi.Ingest{Token: token, Rates: rates, Joins: joins})}
}
