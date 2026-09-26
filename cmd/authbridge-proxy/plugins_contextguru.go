//go:build include_plugin_contextguru

// context-guru belongs to no shipped profile: it is listed as optional in
// scripts/profile-tags, so no artifact links it unless a caller asks
// for it. Its embedded engine pulls a large transitive set (bifrost/core,
// tiktoken-go, tree-sitter grammars, starlark) — roughly 16 MiB on top of a
// proxy binary — which is why it is opt-in rather than profiled.
package main

import _ "github.com/rossoctl/cortex/core/plugins/contextguru"
