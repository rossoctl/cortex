//go:build include_plugin_sessionbudget

// session-budget is opt-IN: it needs the core/storage/redis module (go-redis).
// That module requires core, so this package cannot import it. Each binary
// links the driver in its own storage_redis.go, gated on this same tag.
package all

import _ "github.com/rossoctl/cortex/core/plugins/sessionbudget"
