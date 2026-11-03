//go:build include_plugin_sessionbudget

package main

// The session-budget plugin's Redis driver. It is its own module, which requires
// core, so core/plugins/all cannot link it. Without it, session-budget fails at
// Configure with `storage: unknown scheme "redis"`.
import _ "github.com/rossoctl/cortex/core/storage/redis"
