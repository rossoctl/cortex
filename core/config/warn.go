package config

import (
	"log/slog"
)

// WarnEmptyPipelines emits a startup WARN for every pipeline stage that has
// no plugins. An empty stage is a supported configuration (pass-through),
// not an error, but it means traffic on that direction flows without
// authentication, JWT validation, or token exchange. Surfacing this once
// at startup makes the open-proxy condition visible in logs without
// blocking deployments that intentionally run without plugins (e.g.
// testing, asymmetric deployments).
//
// Call this from each cmd entry point AFTER Validate succeeds and BEFORE
// the pipelines are built. Pass slog.Default() unless you need a scoped
// logger.
func WarnEmptyPipelines(cfg *Config, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(cfg.Pipeline.Inbound.Plugins) == 0 {
		logger.Warn("inbound pipeline has no plugins — incoming traffic will pass through without authentication")
	}
	if len(cfg.Pipeline.Outbound.Plugins) == 0 {
		logger.Warn("outbound pipeline has no plugins — outgoing traffic will pass through without token exchange")
	}
}
