//go:build include_plugin_sparc

// Named plugins_sparcplugin.go, not plugins_sparc.go: `sparc` is a GOARCH token,
// so Go would read the shorter name as an implicit GOARCH=sparc constraint and
// exclude the file on every other architecture — silently dropping the plugin
// with no build error.
package main

import _ "github.com/rossoctl/cortex/core/plugins/sparc"
