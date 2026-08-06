//go:build include_plugin_tokenbudget

// token-budget is opt-IN: it pulls the storage/redis module (go-redis)
// into the binary. Build with -tags include_plugin_tokenbudget to link it.
package main

import (
	_ "github.com/rossoctl/cortex/authbridge/authlib/plugins/tokenbudget"
	_ "github.com/rossoctl/cortex/authbridge/storage/redis"
)
