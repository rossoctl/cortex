//go:build include_plugin_contextguru

// context-guru is opt-IN (positive build tag), unlike the other plugins which are
// compiled in by default and dropped via exclude_plugin_*. Its embedded engine
// pulls a large transitive set (bifrost/core, tiktoken-go, tree-sitter grammars,
// starlark), so it is kept out of the default authbridge-proxy/-envoy binaries and
// linked only when built with -tags include_plugin_contextguru.
package main

import _ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/contextguru"
