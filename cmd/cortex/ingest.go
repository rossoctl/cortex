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

// ingestOptions turns on the ingestion endpoint in praxis mode. In direct mode it returns
// nil and reads neither the environment nor a file.
func ingestOptions(cfg *config.Config, rates pricing.Resolver, getenv func(string) string, readFile func(string) ([]byte, error)) ([]sessionapi.Option, error) {
	if !cfg.PraxisEnabled() || !cfg.Listener.BindLoopbackOnly {
		return nil, nil
	}
	path := getenv(gatewayTokenEnv)
	if path == "" {
		return nil, fmt.Errorf("praxis.enabled needs %s to name the gateway token file", gatewayTokenEnv)
	}
	b, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the gateway token file (%s): %w", gatewayTokenEnv, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("the gateway token file %s (%s) is empty", path, gatewayTokenEnv)
	}
	return []sessionapi.Option{sessionapi.WithIngest(sessionapi.Ingest{Token: token, Rates: rates})}, nil
}
