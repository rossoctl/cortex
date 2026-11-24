//go:build include_plugin_contextguru

// context-guru ships in the local, full and fullcpex profiles (see
// scripts/profile-tags), because the --local config enables it by default. Its
// embedded engine pulls a large transitive set (bifrost/core, tiktoken-go,
// tree-sitter grammars, starlark) — roughly 16 MiB on top of a proxy binary —
// so the lite, envoy and cpex images still leave it out.
package all

import _ "github.com/rossoctl/cortex/core/plugins/contextguru"
