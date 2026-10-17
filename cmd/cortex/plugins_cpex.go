//go:build include_plugin_cpex && cpex

package main

// Needs the `cpex` tag as well: the plugin's manager links libcpex_ffi through
// cgo, and without the tag it is a stub that refuses every config.
import _ "github.com/rossoctl/cortex/core/plugins/cpex"
